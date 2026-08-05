package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The ID token is verified rather than trusted, and this file is the verification.
//
// The back channel to the token endpoint is already TLS to an authenticated host, so a strict
// reading of the OpenID Connect specification says the signature need not be checked on a code
// flow. It is checked anyway. The cost is one public-key operation per sign-in; the property
// bought is that a token accepted here really was minted by the issuer the tenant configured,
// which is the claim a security reviewer will ask about and "the transport was TLS" is a
// weaker answer than "the signature verified".
//
// Two algorithm families are supported: RSA with SHA-256/384/512 and ECDSA on P-256. Every
// identity provider a deployment is likely to meet signs with one of them. `none` and the HMAC
// family are refused outright — an HS256 token verified against a public key is the oldest
// JOSE vulnerability there is, and the defence is to never treat a symmetric algorithm as
// acceptable rather than to check the key type after the fact.

// Bounds on what will be parsed, so a hostile response cannot be an allocation.
const (
	maxTokenLength = 16 * 1024
	maxJWKSKeys    = 16
)

// ErrTokenRefused reports an ID token this build will not accept. Every reason collapses into
// it for the caller — an operator being told which of eight checks failed learns nothing they
// can act on, and an attacker learns which half of a guess landed.
var ErrTokenRefused = errors.New("the identity token was refused")

// claims are the ones this build reads. Everything else the provider sent is ignored rather
// than rejected: an identity provider may add a claim at any time, and a verifier that refused
// unknown claims would break on its next release.
type claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    int64    `json:"exp"`
	IssuedAt  int64    `json:"iat"`
	NotBefore int64    `json:"nbf"`
	Nonce     string   `json:"nonce"`

	Email         string  `json:"email"`
	EmailVerified anyBool `json:"email_verified"`
	Name          string  `json:"name"`
	PreferredName string  `json:"preferred_username"`

	// raw keeps the whole document so the configured group claim can be read from it by name.
	raw map[string]any
}

// audience is one string or several. The specification permits both and providers use both,
// so a decoder that handled one would fail against half the market.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var several []string
	if err := json.Unmarshal(data, &several); err != nil {
		return err
	}
	*a = several
	return nil
}

func (a audience) contains(wanted string) bool {
	for _, entry := range a {
		if entry == wanted {
			return true
		}
	}
	return false
}

// anyBool reads a claim providers send as both a boolean and a string. Microsoft Entra sends
// "true"; Google sends true. Refusing either would mean refusing a working provider over a
// JSON type.
type anyBool bool

func (b *anyBool) UnmarshalJSON(data []byte) error {
	var boolean bool
	if err := json.Unmarshal(data, &boolean); err == nil {
		*b = anyBool(boolean)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*b = anyBool(strings.EqualFold(text, "true"))
	return nil
}

// jwks is an issuer's published signing keys.
type jwks struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kind      string `json:"kty"`
	KeyID     string `json:"kid"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	// RSA.
	Modulus  string `json:"n"`
	Exponent string `json:"e"`
	// EC.
	Curve string `json:"crv"`
	X     string `json:"x"`
	Y     string `json:"y"`
}

// verifyIDToken checks the signature and every claim this build depends on, and returns what
// the provider asserted.
//
// expected is what the flow that started this sign-in recorded. Checking the nonce against the
// flow rather than against anything in the request is the whole point of storing it: a token
// minted for a different authorization request cannot be replayed into this one.
type expectation struct {
	issuer   string
	clientID string
	nonce    string
	now      time.Time
	// leeway absorbs clock skew between this process and the issuer. Sixty seconds is the
	// conventional allowance; larger would extend the life of a stolen token for no gain.
	leeway time.Duration
}

func verifyIDToken(token string, keys jwks, expected expectation) (claims, error) {
	if len(token) > maxTokenLength {
		return claims{}, fmt.Errorf("%w: it is longer than %d bytes",
			ErrTokenRefused, maxTokenLength)
	}
	header, payload, signature, signed, err := splitToken(token)
	if err != nil {
		return claims{}, err
	}

	key, err := signingKey(keys, header, expected)
	if err != nil {
		return claims{}, err
	}
	if err := verifySignature(header.Algorithm, key, signed, signature); err != nil {
		return claims{}, err
	}
	if err := checkClaims(payload, expected); err != nil {
		return claims{}, err
	}
	return payload, nil
}

type joseHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

func splitToken(token string) (joseHeader, claims, []byte, []byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: it is not a compact JWS", ErrTokenRefused)
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the header is not base64url", ErrTokenRefused)
	}
	var header joseHeader
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the header is not JSON", ErrTokenRefused)
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the payload is not base64url", ErrTokenRefused)
	}
	var payload claims
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the payload is not JSON", ErrTokenRefused)
	}
	if err := json.Unmarshal(rawPayload, &payload.raw); err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the payload is not an object", ErrTokenRefused)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return joseHeader{}, claims{}, nil, nil,
			fmt.Errorf("%w: the signature is not base64url", ErrTokenRefused)
	}
	return header, payload, signature, []byte(parts[0] + "." + parts[1]), nil
}

// signingKey resolves which published key signed this token.
//
// A token naming no key identifier is matched against the single published key, if there is
// exactly one. With several published and no identifier there is no way to choose, and trying
// each in turn would mean a token verified by whichever key happened to work — which is not
// what "this issuer signed it" means.
func signingKey(keys jwks, header joseHeader, expected expectation) (any, error) {
	if len(keys.Keys) == 0 || len(keys.Keys) > maxJWKSKeys {
		return nil, fmt.Errorf("%w: the issuer published %d signing keys",
			ErrTokenRefused, len(keys.Keys))
	}
	if !acceptableAlgorithm(header.Algorithm) {
		// `none` and the HMAC family land here. An HS256 token verified against a public key
		// is the oldest JOSE vulnerability there is, and refusing the algorithm outright is
		// the defence — checking the key type afterwards is the version that keeps failing.
		return nil, fmt.Errorf("%w: %q is not a signature algorithm this build accepts",
			ErrTokenRefused, header.Algorithm)
	}

	candidates := make([]jwk, 0, len(keys.Keys))
	for _, key := range keys.Keys {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if header.KeyID != "" && key.KeyID != header.KeyID {
			continue
		}
		if key.Algorithm != "" && key.Algorithm != header.Algorithm {
			continue
		}
		candidates = append(candidates, key)
	}
	if len(candidates) != 1 {
		return nil, fmt.Errorf("%w: %d of the issuer's keys match its header",
			ErrTokenRefused, len(candidates))
	}
	_ = expected
	return publicKeyOf(candidates[0])
}

func acceptableAlgorithm(algorithm string) bool {
	switch algorithm {
	case "RS256", "RS384", "RS512", "ES256":
		return true
	default:
		return false
	}
}

func publicKeyOf(key jwk) (any, error) {
	switch key.Kind {
	case "RSA":
		modulus, err := base64.RawURLEncoding.DecodeString(key.Modulus)
		if err != nil {
			return nil, fmt.Errorf("%w: the key's modulus is not base64url", ErrTokenRefused)
		}
		exponent, err := base64.RawURLEncoding.DecodeString(key.Exponent)
		if err != nil {
			return nil, fmt.Errorf("%w: the key's exponent is not base64url", ErrTokenRefused)
		}
		// A modulus under 2048 bits is refused. It is not a theoretical bound: a provider
		// publishing one is either misconfigured or is not the provider.
		if len(modulus) < 256 {
			return nil, fmt.Errorf("%w: the key is shorter than 2048 bits", ErrTokenRefused)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(modulus),
			E: int(new(big.Int).SetBytes(exponent).Int64()),
		}, nil
	case "EC":
		if key.Curve != "P-256" {
			return nil, fmt.Errorf("%w: %q is not a curve this build accepts",
				ErrTokenRefused, key.Curve)
		}
		x, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil {
			return nil, fmt.Errorf("%w: the key's x is not base64url", ErrTokenRefused)
		}
		y, err := base64.RawURLEncoding.DecodeString(key.Y)
		if err != nil {
			return nil, fmt.Errorf("%w: the key's y is not base64url", ErrTokenRefused)
		}
		return &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q is not a key type this build accepts",
			ErrTokenRefused, key.Kind)
	}
}

func verifySignature(algorithm string, key any, signed, signature []byte) error {
	switch algorithm {
	case "RS256", "RS384", "RS512":
		public, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: the algorithm and the key disagree", ErrTokenRefused)
		}
		hash, digest := digestFor(algorithm, signed)
		if err := rsa.VerifyPKCS1v15(public, hash, digest, signature); err != nil {
			return fmt.Errorf("%w: the signature does not verify", ErrTokenRefused)
		}
		return nil
	case "ES256":
		public, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: the algorithm and the key disagree", ErrTokenRefused)
		}
		// JWS carries r and s as fixed-width halves rather than as the ASN.1 sequence the
		// standard library's Verify expects, so the pair is read out by position.
		if len(signature) != 64 {
			return fmt.Errorf("%w: an ES256 signature is 64 bytes", ErrTokenRefused)
		}
		_, digest := digestFor(algorithm, signed)
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		if !ecdsa.Verify(public, digest, r, s) {
			return fmt.Errorf("%w: the signature does not verify", ErrTokenRefused)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q is not a signature algorithm this build accepts",
			ErrTokenRefused, algorithm)
	}
}

func digestFor(algorithm string, signed []byte) (crypto.Hash, []byte) {
	switch algorithm {
	case "RS384":
		sum := sha512.Sum384(signed)
		return crypto.SHA384, sum[:]
	case "RS512":
		sum := sha512.Sum512(signed)
		return crypto.SHA512, sum[:]
	default:
		sum := sha256.Sum256(signed)
		return crypto.SHA256, sum[:]
	}
}

// checkClaims refuses every token that verified but is not the one this flow asked for.
//
// The issuer must be exactly the one configured — not a prefix, not a host match — because a
// provider that can be talked into issuing for a neighbouring issuer string is how one tenant's
// provider signs another's users in. The audience must contain this client, or a token minted
// for a different application at the same issuer would be accepted here. The nonce must be the
// one this flow recorded, which is what stops a token from another authorization request being
// replayed into this one.
func checkClaims(payload claims, expected expectation) error {
	switch {
	case payload.Issuer != expected.issuer:
		return fmt.Errorf("%w: it was issued by somebody else", ErrTokenRefused)
	case strings.TrimSpace(payload.Subject) == "":
		return fmt.Errorf("%w: it names no subject", ErrTokenRefused)
	case !payload.Audience.contains(expected.clientID):
		return fmt.Errorf("%w: it was minted for a different client", ErrTokenRefused)
	case payload.Nonce != expected.nonce:
		return fmt.Errorf("%w: it does not answer this sign-in", ErrTokenRefused)
	}

	now := expected.now
	if payload.Expiry == 0 {
		return fmt.Errorf("%w: it never expires", ErrTokenRefused)
	}
	if now.After(time.Unix(payload.Expiry, 0).Add(expected.leeway)) {
		return fmt.Errorf("%w: it has expired", ErrTokenRefused)
	}
	if payload.NotBefore != 0 &&
		now.Add(expected.leeway).Before(time.Unix(payload.NotBefore, 0)) {
		return fmt.Errorf("%w: it is not valid yet", ErrTokenRefused)
	}
	if payload.IssuedAt != 0 &&
		now.Add(expected.leeway).Before(time.Unix(payload.IssuedAt, 0)) {
		return fmt.Errorf("%w: it was issued in the future", ErrTokenRefused)
	}
	return nil
}

// groupsFrom reads the configured group claim out of whatever shape the provider used. A list
// of strings is the common one; a single string and a list of objects with a name both occur.
func (c claims) groupsFrom(claim string) []string {
	if claim == "" {
		claim = "groups"
	}
	value, present := c.raw[claim]
	if !present {
		return nil
	}
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		groups := make([]string, 0, len(typed))
		for _, entry := range typed {
			switch member := entry.(type) {
			case string:
				groups = append(groups, member)
			case map[string]any:
				if name, ok := member["name"].(string); ok {
					groups = append(groups, name)
				}
			}
		}
		return groups
	default:
		return nil
	}
}

// displayName is what the record will call this person. The provider's own name claim wins;
// the preferred username and the local part of the address are the fallbacks, because an event
// naming a raw subject identifier is one nobody reading an audit trail can act on.
func (c claims) displayName() string {
	for _, candidate := range []string{c.Name, c.PreferredName} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	if local, _, found := strings.Cut(c.Email, "@"); found && local != "" {
		return local
	}
	return c.Subject
}

package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Bounds on what an identity provider may cost this process. Every one of them exists because
// the endpoints are named by an administrator and answered by somebody else's server.
const (
	discoveryTimeout  = 10 * time.Second
	exchangeTimeout   = 15 * time.Second
	maxProviderBody   = 512 * 1024
	discoveryCacheTTL = 10 * time.Minute
	// jwksCacheTTL is shorter than the discovery cache because a key rotation is the thing a
	// stale cache breaks, and a provider rotating keys does not announce it.
	jwksCacheTTL = 5 * time.Minute
)

// ErrProviderUnreachable reports an identity provider that could not be spoken to. It is
// separate from a refusal because the operator's next action differs: one is "your provider is
// down", the other is "your provider said no".
var ErrProviderUnreachable = errors.New("the identity provider could not be reached")

// discovery is the part of an issuer's metadata this build uses.
type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// OIDC speaks the Authorization Code flow with PKCE to whatever an administrator configured.
//
// It caches discovery documents and signing keys, because an identity provider's metadata
// changes rarely and fetching it on every sign-in would put a third party's latency and
// availability directly in front of every operator signing in.
type OIDC struct {
	// Client is the HTTP client used against providers. It is a field so a test can serve a
	// mock issuer, and so a deployment can put a proxy or a pinned TLS configuration in front
	// of every outbound identity call.
	Client *http.Client

	mutex    sync.Mutex
	metadata map[string]cached[discovery]
	signing  map[string]cached[jwks]
}

type cached[T any] struct {
	value   T
	fetched time.Time
}

// NewOIDC builds a client with the timeouts a provider call is bounded by.
func NewOIDC() *OIDC {
	return &OIDC{
		Client: &http.Client{
			Timeout: exchangeTimeout,
			// A redirect chain from a provider endpoint is not something this flow needs, and
			// following one is how a misconfigured issuer becomes a request to somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		metadata: make(map[string]cached[discovery]),
		signing:  make(map[string]cached[jwks]),
	}
}

// AuthorizationRequest is what a sign-in needs to send the browser away with, and what has to
// be remembered until it comes back.
type AuthorizationRequest struct {
	// URL is where the browser goes.
	URL string
	// State, CodeVerifier and Nonce are recorded server-side. Only the state reaches the
	// browser; the other two never leave this process, which is what makes checking them
	// meaningful rather than ceremonial.
	State        string
	CodeVerifier string
	Nonce        string
}

// Authorize builds the authorization request for one provider.
func (o *OIDC) Authorize(
	ctx context.Context, issuer, clientID, redirectURI string, scopes []string,
) (AuthorizationRequest, error) {
	metadata, err := o.discover(ctx, issuer)
	if err != nil {
		return AuthorizationRequest{}, err
	}

	state, err := randomToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	nonce, err := randomToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}

	// S256 rather than `plain`. A plain challenge is the verifier, so an attacker who
	// intercepted the authorization request would hold everything needed to redeem the code.
	challenge := sha256.Sum256([]byte(verifier))

	endpoint, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return AuthorizationRequest{}, fmt.Errorf(
			"%w: its authorization endpoint is not a URL", ErrProviderUnreachable)
	}
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", strings.Join(scopes, " "))
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	endpoint.RawQuery = query.Encode()

	return AuthorizationRequest{
		URL:          endpoint.String(),
		State:        state,
		CodeVerifier: verifier,
		Nonce:        nonce,
	}, nil
}

// Exchange redeems an authorization code and returns what the provider asserted about the
// person, having verified the token it came in.
func (o *OIDC) Exchange(
	ctx context.Context, issuer, clientID, clientSecret, redirectURI, code, verifier, nonce string,
) (claims, error) {
	metadata, err := o.discover(ctx, issuer)
	if err != nil {
		return claims{}, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)

	exchangeCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		exchangeCtx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return claims{}, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	// client_secret_basic rather than a form field. It is what the specification names as the
	// default, it is what every provider accepts, and it keeps the secret out of a body that
	// intermediaries log.
	request.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))

	body, err := o.fetch(request)
	if err != nil {
		return claims{}, err
	}

	var answer struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return claims{}, fmt.Errorf("%w: its token endpoint did not answer JSON",
			ErrProviderUnreachable)
	}
	if answer.Error != "" {
		// A replayed authorization code lands here: the provider itself refuses the second
		// redemption. This build ALSO refuses it one layer earlier, because the sign-in flow is
		// consumed by a conditional update — the two together are why a replay cannot work
		// even against a provider that would tolerate one.
		return claims{}, fmt.Errorf("%w: the provider refused the authorization code",
			ErrTokenRefused)
	}
	if answer.IDToken == "" {
		return claims{}, fmt.Errorf("%w: its token endpoint returned no identity token",
			ErrTokenRefused)
	}

	keys, err := o.keys(ctx, metadata)
	if err != nil {
		return claims{}, err
	}
	return verifyIDToken(answer.IDToken, keys, expectation{
		issuer:   metadata.Issuer,
		clientID: clientID,
		nonce:    nonce,
		now:      time.Now(),
		leeway:   time.Minute,
	})
}

// discover reads an issuer's metadata, from cache when it is fresh.
func (o *OIDC) discover(ctx context.Context, issuer string) (discovery, error) {
	if err := usableIssuer(issuer); err != nil {
		return discovery{}, err
	}

	o.mutex.Lock()
	held, ok := o.metadata[issuer]
	o.mutex.Unlock()
	if ok && time.Since(held.fetched) < discoveryCacheTTL {
		return held.value, nil
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	address := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	request, err := http.NewRequestWithContext(discoveryCtx, http.MethodGet, address, nil)
	if err != nil {
		return discovery{}, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	request.Header.Set("Accept", "application/json")

	body, err := o.fetch(request)
	if err != nil {
		return discovery{}, err
	}
	var metadata discovery
	if err := json.Unmarshal(body, &metadata); err != nil {
		return discovery{}, fmt.Errorf("%w: its metadata is not JSON", ErrProviderUnreachable)
	}
	// The metadata must claim the issuer it was fetched from. Without this check an issuer
	// that could be talked into serving somebody else's metadata would hand this process
	// somebody else's endpoints, and every later check would pass against the wrong provider.
	if strings.TrimSuffix(metadata.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return discovery{}, fmt.Errorf(
			"%w: its metadata claims a different issuer", ErrProviderUnreachable)
	}
	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" ||
		metadata.JWKSURI == "" {
		return discovery{}, fmt.Errorf(
			"%w: its metadata is missing an endpoint this flow needs", ErrProviderUnreachable)
	}

	o.mutex.Lock()
	o.metadata[issuer] = cached[discovery]{value: metadata, fetched: time.Now()}
	o.mutex.Unlock()
	return metadata, nil
}

// keys reads an issuer's signing keys, from cache when they are fresh.
func (o *OIDC) keys(ctx context.Context, metadata discovery) (jwks, error) {
	o.mutex.Lock()
	held, ok := o.signing[metadata.JWKSURI]
	o.mutex.Unlock()
	if ok && time.Since(held.fetched) < jwksCacheTTL {
		return held.value, nil
	}

	keysCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(keysCtx, http.MethodGet, metadata.JWKSURI, nil)
	if err != nil {
		return jwks{}, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	request.Header.Set("Accept", "application/json")

	body, err := o.fetch(request)
	if err != nil {
		return jwks{}, err
	}
	var keys jwks
	if err := json.Unmarshal(body, &keys); err != nil {
		return jwks{}, fmt.Errorf("%w: its signing keys are not JSON", ErrProviderUnreachable)
	}

	o.mutex.Lock()
	o.signing[metadata.JWKSURI] = cached[jwks]{value: keys, fetched: time.Now()}
	o.mutex.Unlock()
	return keys, nil
}

// fetch performs one bounded call against a provider. The body limit is what stops a hostile
// or broken endpoint from being an allocation in this process.
func (o *OIDC) fetch(request *http.Request) ([]byte, error) {
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	// A token endpoint answers 400 with a JSON error document on a refused code, and that is a
	// refusal rather than an outage, so the body is returned for the caller to read.
	if response.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: it answered %d", ErrProviderUnreachable, response.StatusCode)
	}
	return body, nil
}

// usableIssuer refuses an issuer this process will not call.
//
// HTTPS is required, because everything about this flow rests on the back channel being to the
// host it claims to be. The one exception is a loopback address, which is what a local mock
// issuer is and what a developer's own provider is; it cannot be reached from outside the
// host, so it is not a way to downgrade a real deployment.
func usableIssuer(issuer string) error {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: %q is not a URL", ErrProviderUnreachable, issuer)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && isLoopback(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: %q is not https", ErrProviderUnreachable, issuer)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// randomToken mints a value used as a state, a nonce or a PKCE verifier. 32 bytes encodes to
// 43 characters, which is exactly the minimum length RFC 7636 requires of a verifier.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: minting a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

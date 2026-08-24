package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/session"
)

// The identity tests run against a LOCAL MOCK ISSUER rather than a live provider, and the
// reason is worth stating: what is under test is this control plane's handling of PKCE, state,
// nonce, code replay and the tenant's provisioning policy. A live provider would test the
// provider, would need a credential in CI, and could not be made to answer wrongly on purpose —
// which is what most of these cases need it to do.
//
// The issuer signs with a real RSA key and publishes a real JWKS, so the signature verification
// path is exercised rather than stubbed. It can be told to misbehave in each of the specific
// ways an attacker would need it to.

const (
	identityOrg       = "org-a"
	identityNeighbour = "org-neighbour"
	identityToken     = "an-operator-bootstrap-token-long-enough"
	identityConsole   = "https://console.example.test"
)

// mockIssuer is an OpenID Connect provider that can be made to answer wrongly.
type mockIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu sync.Mutex
	// claims is what the next identity token asserts. A test rewrites it to change who is
	// signing in and what the provider says about them.
	claims map[string]any
	// redeemed is every authorization code this issuer has already exchanged, so it can refuse
	// a second redemption the way a real provider does.
	redeemed map[string]bool
	// challenges maps an authorization code to the PKCE challenge the request carried, so the
	// verifier presented at redemption can be checked.
	challenges map[string]string
	// refuseVerifier makes the issuer accept a mismatched verifier, so a test can prove that
	// THIS control plane's own defences are what refuse a replay rather than the provider's.
	refuseVerifier bool
	// audience overrides who the token is minted for.
	audience string
	// signWithAnotherKey mints the token under a key the issuer never published.
	signWithAnotherKey *rsa.PrivateKey
}

func newMockIssuer(t *testing.T) *mockIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating an issuer key: %v", err)
	}
	issuer := &mockIssuer{
		key:            key,
		redeemed:       make(map[string]bool),
		challenges:     make(map[string]string),
		refuseVerifier: true,
		claims: map[string]any{
			"sub":            "operator-1",
			"email":          "ada@example.test",
			"email_verified": true,
			"name":           "Ada Lovelace",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", issuer.metadata)
	mux.HandleFunc("GET /jwks", issuer.jwks)
	mux.HandleFunc("GET /authorize", issuer.authorize)
	mux.HandleFunc("POST /token", issuer.token)

	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (m *mockIssuer) url() string { return m.server.URL }

func (m *mockIssuer) assert(t *testing.T, claim string, value any) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims[claim] = value
}

func (m *mockIssuer) metadata(writer http.ResponseWriter, _ *http.Request) {
	writeIssuerJSON(writer, map[string]any{
		"issuer":                 m.server.URL,
		"authorization_endpoint": m.server.URL + "/authorize",
		"token_endpoint":         m.server.URL + "/token",
		"jwks_uri":               m.server.URL + "/jwks",
	})
}

func (m *mockIssuer) jwks(writer http.ResponseWriter, _ *http.Request) {
	writeIssuerJSON(writer, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": "issuer-key-1",
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(m.key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(m.key.E)).Bytes()),
	}}})
}

// authorize records the PKCE challenge against a fresh code and redirects back, exactly as a
// provider does once a person has authenticated.
func (m *mockIssuer) authorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()

	code := "code-" + strings.TrimPrefix(query.Get("state"), "")[:8]
	m.mu.Lock()
	m.challenges[code] = query.Get("code_challenge")
	m.claims["nonce"] = query.Get("nonce")
	m.mu.Unlock()

	back, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		http.Error(writer, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	returning := back.Query()
	returning.Set("code", code)
	returning.Set("state", query.Get("state"))
	back.RawQuery = returning.Encode()

	http.Redirect(writer, request, back.String(), http.StatusFound)
}

func (m *mockIssuer) token(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeIssuerJSON(writer, map[string]any{"error": "invalid_request"})
		return
	}
	code := request.Form.Get("code")

	m.mu.Lock()
	alreadyUsed := m.redeemed[code]
	challenge := m.challenges[code]
	refuseVerifier := m.refuseVerifier
	audience := m.audience
	other := m.signWithAnotherKey
	claims := make(map[string]any, len(m.claims))
	for key, value := range m.claims {
		claims[key] = value
	}
	m.redeemed[code] = true
	m.mu.Unlock()

	if alreadyUsed {
		writeIssuerJSON(writer, map[string]any{"error": "invalid_grant"})
		return
	}
	if refuseVerifier {
		presented := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(presented[:]) != challenge {
			writeIssuerJSON(writer, map[string]any{"error": "invalid_grant"})
			return
		}
	}

	clientID, _, _ := request.BasicAuth()
	if unescaped, err := url.QueryUnescape(clientID); err == nil {
		clientID = unescaped
	}
	claims["iss"] = m.server.URL
	claims["aud"] = clientID
	if audience != "" {
		claims["aud"] = audience
	}
	claims["exp"] = time.Now().Add(5 * time.Minute).Unix()
	claims["iat"] = time.Now().Unix()

	signing := m.key
	if other != nil {
		signing = other
	}
	writeIssuerJSON(writer, map[string]any{"id_token": signRS256(claims, signing)})
}

// signRS256 mints a compact JWS. It is spelled out rather than pulled from a library so the
// test can produce a token that is wrong in exactly one way.
func signRS256(claims map[string]any, key *rsa.PrivateKey) string {
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "issuer-key-1"})
	payload, _ := json.Marshal(claims)

	signed := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return signed + ".unsigned"
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func writeIssuerJSON(writer http.ResponseWriter, body any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(body)
}

// identityPlane is a control plane with the operator surface, a bootstrap credential bound to
// one organization, and a console origin the CSRF check will accept.
type identityPlane struct {
	*controlPlane
	operator string
	dsn      string
}

func startIdentityPlane(t *testing.T) *identityPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(identityToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = identityOrg
		cfg.OperatorPublicURL = "http://" + operatorAddress
		cfg.OperatorConsoleURL = identityConsole
		cfg.OperatorAllowedOrigins = []string{identityConsole}
		// A key, so a provider's client secret can be held at all. Without one, configuring a
		// provider is refused rather than stored in the clear — which is itself asserted below.
		cfg.SealingKey = make([]byte, 32)
		for index := range cfg.SealingKey {
			cfg.SealingKey[index] = byte(index + 1)
		}
		// The neighbour shares this database deliberately. An organization with no database
		// fails before any query runs, which would leave the cross-tenant assertions passing
		// against an implementation with no scoping at all.
		dsn = cfg.DatabaseDSN
	})
	identity := &identityPlane{controlPlane: plane, operator: operatorAddress, dsn: dsn}
	identity.waitForOperatorSurface(t)
	return identity
}

// waitForOperatorSurface blocks until the operator listener answers.
//
// startControlPlane returns as soon as the HEALTH listener is up, and the operator surface is a
// separate listener that binds afterwards. Without this the first request in a test races that
// bind and fails as a refused connection — which reads as a product defect and is a harness
// one. It also turns a genuine failure to assemble the surface into the reason for it rather
// than a dial error, because the logs are printed.
func (p *identityPlane) waitForOperatorSurface(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", p.operator, time.Second)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the operator surface never listened on %s\nlogs:\n%s",
				p.operator, p.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *identityPlane) base(organization string) string {
	return "http://" + p.operator + "/operator/v1/organizations/" + organization
}

// answer is one exchange with the operator surface, as a caller observes it.
type answer struct {
	status  int
	body    string
	cookies []*http.Cookie
	// location is where a redirect pointed, which is the whole observable result of a sign-in.
	location string
}

// call makes one request as whatever credential the caller names.
//
// The Origin header is set on every unsafe request, because a browser sets it on every unsafe
// request; a test that omitted it would be asserting the CSRF check rather than the thing it
// meant to assert. The one case that omits it deliberately says so.
func (p *identityPlane) call(
	t *testing.T, method, url string, body any, credential ...func(*http.Request),
) answer {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}
		payload = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		request.Header.Set("Origin", identityConsole)
	}
	for _, apply := range credential {
		apply(request)
	}

	// Redirects are not followed: where the surface sent the browser is the observable result
	// of a sign-in, and following it would land on a console that does not exist here.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("calling %s %s: %v", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return answer{
		status:   response.StatusCode,
		body:     string(raw),
		cookies:  response.Cookies(),
		location: response.Header.Get("Location"),
	}
}

// asBootstrap presents the configured bootstrap credential.
func asBootstrap(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+identityToken)
}

// asToken presents an issued API token.
func asToken(secret string) func(*http.Request) {
	return func(request *http.Request) {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
}

// asSession presents a session cookie.
func asSession(token string) func(*http.Request) {
	return func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	}
}

// withoutOrigin removes the Origin header, so a test can assert the CSRF check rather than
// silently satisfy it.
func withoutOrigin(request *http.Request) { request.Header.Del("Origin") }

// sessionCookie reads the opaque credential out of a response, or reports that there was none.
func sessionCookie(t *testing.T, from answer) string {
	t.Helper()
	for _, cookie := range from.cookies {
		if cookie.Name == session.CookieName && cookie.Value != "" {
			return cookie.Value
		}
	}
	t.Fatalf("no session cookie was issued: %d %s", from.status, from.body)
	return ""
}

// decodeAnswer reads a response body into a shape, failing with the body when it will not fit.
func decodeAnswer(t *testing.T, from answer, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(from.body), into); err != nil {
		t.Fatalf("decoding %d %s: %v", from.status, from.body, err)
	}
}

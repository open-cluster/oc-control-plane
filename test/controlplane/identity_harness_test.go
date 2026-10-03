package controlplane

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

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

const (
	identityOrg       = "11111111-1111-4111-8111-111111111111"
	identityNeighbour = "22222222-2222-4222-8222-222222222222"
	identityToken     = "an-operator-bootstrap-token-long-enough"
)

type mockIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu                 sync.Mutex
	claims             map[string]any
	redeemed           map[string]bool
	challenges         map[string]string
	refuseVerifier     bool
	audience           string
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

	clientID := request.Form.Get("client_id")
	if basicClientID, _, ok := request.BasicAuth(); ok && basicClientID != "" {
		clientID = basicClientID
	}
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
	writeIssuerJSON(writer, map[string]any{
		"access_token": "mock-access-token", "token_type": "Bearer", "expires_in": 300,
		"id_token": signRS256(claims, signing),
	})
}

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

type identityPlane struct {
	*controlPlane
	api string
	dsn string
}

func startIdentityPlane(t *testing.T, configure ...func(*config.Config)) *identityPlane {
	t.Helper()

	apiAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		digest := sha256.Sum256([]byte(identityToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.PublicURL = "http://" + apiAddress
		cfg.SealingKey = make([]byte, 32)
		for index := range cfg.SealingKey {
			cfg.SealingKey[index] = byte(index + 1)
		}
		dsn = cfg.DatabaseDSN
		for _, apply := range configure {
			apply(cfg)
		}
	})
	identity := &identityPlane{controlPlane: plane, api: apiAddress, dsn: dsn}
	identity.waitForAPISurface(t)
	return identity
}

func (p *identityPlane) waitForAPISurface(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", p.api, time.Second)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the application API never listened on %s\nlogs:\n%s",
				p.api, p.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *identityPlane) base(organization string) string {
	return "http://" + p.api + "/api/v1"
}

type answer struct {
	status   int
	body     string
	cookies  []*http.Cookie
	location string
}

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
		request.Header.Set("Origin", "http://"+p.api)
	}
	for _, apply := range credential {
		apply(request)
	}

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

func asBootstrap(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+identityToken)
}

func asSession(token string) func(*http.Request) {
	return func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	}
}

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

func decodeAnswer(t *testing.T, from answer, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(from.body), into); err != nil {
		t.Fatalf("decoding %d %s: %v", from.status, from.body, err)
	}
}

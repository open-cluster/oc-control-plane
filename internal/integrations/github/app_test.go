package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The App credential machinery against a fake GitHub: the JWT this build signs is one
// GitHub would accept, and installation tokens are minted once and reused until near
// expiry rather than on every read.

// testKey generates an RSA key once per test that needs one. 1024 bits is far below any
// deployment's key and exactly enough for a signature round-trip under test.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	return key
}

func pemPKCS1(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func pemPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestNewAppReadsBothPEMShapes(t *testing.T) {
	t.Parallel()

	key := testKey(t)
	for name, encoded := range map[string][]byte{
		"pkcs1": pemPKCS1(key),
		"pkcs8": pemPKCS8(t, key),
	} {
		if _, err := NewApp("12345", encoded, NewClient("")); err != nil {
			t.Errorf("a %s key was refused: %v", name, err)
		}
	}
}

func TestNewAppRefusesAnUnusableKeyWithoutQuotingIt(t *testing.T) {
	t.Parallel()

	_, err := NewApp("12345", []byte("not a key at all"), NewClient(""))
	if err == nil {
		t.Fatal("an unreadable key must be refused at startup")
	}
	if strings.Contains(err.Error(), "not a key at all") {
		t.Error("the error quotes the key file's contents")
	}
}

func TestAppJWTIsOneGitHubWouldAccept(t *testing.T) {
	t.Parallel()

	key := testKey(t)
	app, err := NewApp("12345", pemPKCS1(key), NewClient(""))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}

	token, err := app.jwt(time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a compact JWT has three parts, got %d", len(parts))
	}

	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	decodePart(t, parts[0], &header)
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v; GitHub accepts RS256 JWTs", header)
	}

	var claims struct {
		Issuer   string `json:"iss"`
		IssuedAt int64  `json:"iat"`
		Expiry   int64  `json:"exp"`
	}
	decodePart(t, parts[1], &claims)
	if claims.Issuer != "12345" {
		t.Errorf("iss = %q, want the app id", claims.Issuer)
	}
	// Issued in the near past on purpose — GitHub refuses tokens from the future, and a
	// deployment's clock can lead GitHub's by a few seconds.
	if claims.IssuedAt >= claims.Expiry || claims.Expiry-claims.IssuedAt > 600 {
		t.Errorf("iat=%d exp=%d; GitHub caps a JWT at ten minutes", claims.IssuedAt, claims.Expiry)
	}

	// The signature must verify under the public key, or nothing above matters.
	if err := verifyRS256(key, parts[0]+"."+parts[1], parts[2]); err != nil {
		t.Errorf("the signature does not verify: %v", err)
	}
}

func TestInstallationTokensAreCachedUntilNearExpiry(t *testing.T) {
	t.Parallel()

	var minted atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/app/installations/77/access_tokens" {
				t.Errorf("asked for %q", request.URL.Path)
			}
			if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
				t.Error("minting must present the app JWT")
			}
			minted.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			_, _ = writer.Write([]byte(`{"token":"ghs_installation_token","expires_at":"` +
				expiry + `"}`))
		}))
	t.Cleanup(fake.Close)

	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient(fake.URL))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}

	ctx := testContext(t)
	for range 3 {
		token, tokenErr := app.installationToken(ctx, 77)
		if tokenErr != nil {
			t.Fatalf("minting: %v", tokenErr)
		}
		if token != "ghs_installation_token" {
			t.Errorf("token = %q", token)
		}
	}
	if minted.Load() != 1 {
		t.Errorf("three reads minted %d tokens; a mint per read spends the rate budget "+
			"the reads need", minted.Load())
	}
}

func TestAnExpiringInstallationTokenIsReplaced(t *testing.T) {
	t.Parallel()

	var minted atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			minted.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			// Already inside the refresh margin, so every ask mints anew.
			expiry := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
			_, _ = writer.Write([]byte(`{"token":"ghs_short_lived","expires_at":"` +
				expiry + `"}`))
		}))
	t.Cleanup(fake.Close)

	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient(fake.URL))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}

	ctx := testContext(t)
	for range 2 {
		if _, tokenErr := app.installationToken(ctx, 77); tokenErr != nil {
			t.Fatalf("minting: %v", tokenErr)
		}
	}
	if minted.Load() != 2 {
		t.Errorf("a token inside the refresh margin was served from cache %d times",
			2-minted.Load())
	}
}

func TestAnUnconfiguredAppSaysSo(t *testing.T) {
	t.Parallel()

	var app *App
	if app.Configured() {
		t.Error("a nil app claims to be configured")
	}
	_, err := app.installationToken(testContext(t), 77)
	if err == nil || !strings.Contains(err.Error(), "GitHub App") {
		t.Errorf("an unconfigured app must refuse by name, got %v", err)
	}
}

func decodePart(t *testing.T, part string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decoding a JWT part: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("a JWT part is not JSON: %v", err)
	}
}

func verifyRS256(key *rsa.PrivateKey, signed, signature string) error {
	raw, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return err
	}
	digest := sha256Sum([]byte(signed))
	return rsa.VerifyPKCS1v15(&key.PublicKey, cryptoSHA256, digest, raw)
}

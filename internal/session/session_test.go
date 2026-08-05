package session_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/session"
)

// The credential exists in a readable form exactly once. What is stored must not be it, or a
// disclosure of the sessions table is a disclosure of every live session.
func TestTheStoredValueIsNotTheCredential(t *testing.T) {
	t.Parallel()

	token, digest, err := session.NewToken()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if token == "" || len(digest) != 32 {
		t.Fatalf("minted %q with a %d-byte digest", token, len(digest))
	}
	if strings.Contains(string(token), string(digest)) || string(digest) == string(token) {
		t.Error("the digest is the token")
	}
	if string(session.Digest(token)) != string(digest) {
		t.Error("digesting the token again gives a different value; a lookup would never match")
	}

	second, _, err := session.NewToken()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if second == token {
		t.Fatal("two mints produced the same token")
	}
}

// Story 5: a session that has expired must be told apart from one an administrator ended, so
// the interface can say which happened rather than showing a screen of error states.
func TestASessionSaysWhyItMayNotAuthenticate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	for _, testCase := range []struct {
		name    string
		session session.Session
		want    error
	}{
		{"live", session.Session{ExpiresAt: now.Add(time.Hour)}, nil},
		{"expired", session.Session{ExpiresAt: now.Add(-time.Second)}, session.ErrExpired},
		{"revoked", session.Session{
			ExpiresAt: now.Add(time.Hour), RevokedAt: now.Add(-time.Minute),
		}, session.ErrRevoked},
		// Revocation wins. An administrator who ended a session must be told they did rather
		// than that it timed out on its own — the two lead to different next actions.
		{"revoked and expired", session.Session{
			ExpiresAt: now.Add(-time.Hour), RevokedAt: now.Add(-time.Hour),
		}, session.ErrRevoked},
		// The boundary itself: a session expires AT its expiry, not after it.
		{"exactly at expiry", session.Session{ExpiresAt: now}, session.ErrExpired},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.session.Refusal(now); got != testCase.want {
				t.Errorf("refusal = %v, want %v", got, testCase.want)
			}
			if live := testCase.session.Live(now); live != (testCase.want == nil) {
				t.Errorf("Live = %v while the refusal is %v", live, testCase.want)
			}
		})
	}
}

// Story 11: an organization sets its own lifetime. It may tighten past the default and may not
// widen past what this build is willing to serve — a policy is a customer's decision inside a
// product's bounds, not instead of them.
func TestAnOrganizationsLifetimeIsHeldInsideWhatTheBuildServes(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"unset takes the default", 0, session.DefaultLifetime},
		{"negative takes the default", -time.Hour, session.DefaultLifetime},
		{"tighter is honoured", time.Hour, time.Hour},
		{"below the floor is raised", time.Second, session.MinLifetime},
		{"beyond the ceiling is capped", 365 * 24 * time.Hour, session.MaxLifetime},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := session.ClampLifetime(testCase.configured); got != testCase.want {
				t.Errorf("clamped %v to %v, want %v", testCase.configured, got, testCase.want)
			}
		})
	}
}

// The cookie's attributes are the transport half of the design, and every one of them is
// load-bearing: HttpOnly against script, Secure against a plaintext hop, Lax against a
// cross-site post, Path=/ so one cookie serves the surface. The __Host- prefix makes the
// browser enforce three of them too.
func TestTheCookieCarriesEveryAttributeTheDesignDependsOn(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	session.Set(recorder, "opaque-value", time.Now().Add(time.Hour))

	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("wrote %d cookies", len(cookies))
	}
	cookie := cookies[0]

	if cookie.Name != session.CookieName || !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Errorf("the cookie is named %q; the __Host- prefix is what makes the browser enforce "+
			"Secure, Path=/ and no Domain as well as this code", cookie.Name)
	}
	if !cookie.HttpOnly {
		t.Error("the cookie is readable from script")
	}
	if !cookie.Secure {
		t.Error("the cookie may cross a plaintext hop")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite is %v, want Lax; Strict drops the cookie on the sign-in redirect "+
			"back from the identity provider, which is the one hop it must survive",
			cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path is %q, want /", cookie.Path)
	}
	if cookie.Domain != "" {
		t.Errorf("Domain is %q; __Host- forbids one", cookie.Domain)
	}
}

// Sign-out clears the cookie in the same response that deleted the row, so the browser stops
// presenting a credential that is already dead.
func TestClearingTheCookieExpiresItImmediately(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	session.Clear(recorder)

	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("wrote %d cookies", len(cookies))
	}
	if cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Errorf("the cleared cookie is %q with MaxAge %d", cookies[0].Value, cookies[0].MaxAge)
	}
}

func TestARequestWithNoCookiePresentsNothing(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/operator/v1/session", nil)
	if _, present := session.FromRequest(request); present {
		t.Error("a request with no cookie presented a token")
	}

	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: "opaque"})
	token, present := session.FromRequest(request)
	if !present || token != "opaque" {
		t.Errorf("read %q present=%v", token, present)
	}
}

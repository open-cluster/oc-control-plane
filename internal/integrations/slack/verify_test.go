package slack

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func authTestGranting(scopes string) func(http.ResponseWriter, *http.Request) {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-OAuth-Scopes", scopes)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme","team_id":"T0ACME",` +
			`"user":"opencluster-bot","user_id":"U0BOT"}`))
	}
}

func TestProbeWithEveryScopeIsVerifiedAndNamesTheWorkspace(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting(
		"channels:read,channels:history,search:read,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("status = %s, want verified; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "Acme") || !strings.Contains(verified.Note, "opencluster-bot") {
		t.Errorf("the note %q does not say whose workspace and bot answered", verified.Note)
	}

	// The verified grants are on the record — tool availability derives from them —
	// and a bot token never records user_token, so user-token-only search stays absent.
	granted := strings.Join(verified.Grants, " ")
	for _, scope := range []string{"channels:read", "channels:history", "search:read", "users:read"} {
		if !strings.Contains(granted, scope) {
			t.Errorf("grants %v do not record scope %s", verified.Grants, scope)
		}
	}
	if strings.Contains(granted, "user_token") {
		t.Errorf("a bot token recorded user_token: %v", verified.Grants)
	}
}

func TestProbeRecordsAUserTokenAsOne(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("search:read,channels:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxp-a-user-token")
	found := false
	for _, grant := range verified.Grants {
		found = found || grant == "user_token"
	}
	if !found {
		t.Errorf("a user token must record user_token, got %v", verified.Grants)
	}
}

func TestProbeWithAMissingScopeIsVerifiedAndNamesWhatItCosts(t *testing.T) {
	t.Parallel()

	// A scope this product DOES request and the installation did not grant. It used to be
	// search:read here, which is the one scope we never ask for — asserting on it was
	// asserting that a correct installation reads as broken.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("status = %s, want verified; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "users:read") {
		t.Errorf("the note %q does not name the missing scope", verified.Note)
	}
}

func TestProbeWithARefusedTokenIsFailedInTheOperatorsLanguage(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":false,"error":"invalid_auth"}`)

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-revoked")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "invalid_auth") {
		t.Errorf("the note %q does not carry the vendor's own reason", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	// A closed port: the vendor cannot be reached at all, which is a different fact from a
	// refused token and must read as one.
	verified := probe(testContext(t), NewClient("http://127.0.0.1:1"), "xoxb-under-test")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeUnderRateLimitingFails(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "rate limiting") {
		t.Errorf("the note %q does not say what to wait for", verified.Note)
	}
}

func TestProbeWithUnreportedScopesIsVerifiedWithoutGrants(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("status = %s, want verified; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "scopes") {
		t.Errorf("the note %q does not say the grants could not be read", verified.Note)
	}
	if verified.Grants != nil {
		t.Errorf("unreadable scopes must record nothing, got %v", verified.Grants)
	}
}

func TestProbeWithABotTokensOwnScopesIsVerified(t *testing.T) {
	t.Parallel()

	// The recommended bot installation, exactly: every scope the offered tools need and
	// no workspace-wide search, which this product deliberately does not ask for. It
	// reported degraded, which told a customer their correct installation was broken.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("status = %s, want verified; note: %s", verified.Status, verified.Note)
	}
	if strings.Contains(verified.Note, "search:read") {
		t.Errorf("the note %q still holds a scope we never requested against the customer",
			verified.Note)
	}
}

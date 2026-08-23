package slack

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The probe against the fake vendor. What is asserted is the judgement an operator reads:
// a working token is active, a missing grant is degraded and names what stops working, a
// refused token is failed — never an "active" resting on a form having validated.

func authTestGranting(scopes string) func(http.ResponseWriter, *http.Request) {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-OAuth-Scopes", scopes)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme","team_id":"T0ACME",` +
			`"user":"opencluster-bot","user_id":"U0BOT"}`))
	}
}

func TestProbeWithEveryScopeIsActiveAndNamesTheWorkspace(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting(
		"channels:read,channels:history,search:read,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
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

func TestProbeWithAMissingScopeIsDegradedAndNamesWhatItCosts(t *testing.T) {
	t.Parallel()

	// A scope this product DOES request and the installation did not grant. It used to be
	// search:read here, which is the one scope we never ask for — asserting on it was
	// asserting that a correct installation reads as broken.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
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

func TestProbeUnderRateLimitingIsDegradedNotFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	// The vendor answered — it is rate limiting, not refusing the credential — so failed
	// would tell the operator their token died when nothing of the kind is known.
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "rate limiting") {
		t.Errorf("the note %q does not say what to wait for", verified.Note)
	}
}

func TestProbeWithUnreportedScopesIsDegraded(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "scopes") {
		t.Errorf("the note %q does not say the grants could not be read", verified.Note)
	}
	if verified.Grants != nil {
		t.Errorf("unreadable scopes must record nothing, got %v", verified.Grants)
	}
}

func TestProbeWithABotTokensOwnScopesIsActive(t *testing.T) {
	t.Parallel()

	// The recommended bot installation, exactly: every scope the offered tools need and
	// no workspace-wide search, which this product deliberately does not ask for. It
	// reported degraded, which told a customer their correct installation was broken.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
	if strings.Contains(verified.Note, "search:read") {
		t.Errorf("the note %q still holds a scope we never requested against the customer",
			verified.Note)
	}
}

// FACTS: WHAT THE VERIFICATION ESTABLISHED, AS ATTRIBUTES RATHER THAN PROSE.
//
// The probe has always read the workspace and the bot off auth.test and written both into
// the SENTENCE of the note. An operator with three Slack workspaces could not tell from
// the integration page which one they were looking at without parsing a status line, and a
// console had nothing to render as an attribute — which is how a frontend fixture came to
// invent the values and show a customer a surface this service never sends.
//
// Facts are display-only by construction. Nothing here is consulted by an authorization
// decision; scope decisions read Grants, which is what tool availability derives from.

func recordedFacts(t *testing.T, verified integrations.Verification) map[string]any {
	t.Helper()

	if verified.Facts == nil {
		t.Fatalf("the verification recorded no facts at all; note: %s", verified.Note)
	}
	return verified.Facts
}

func assertFact(t *testing.T, facts map[string]any, key, want string) {
	t.Helper()

	got, present := facts[key]
	if !present {
		t.Errorf("no %q fact was recorded; facts: %+v", key, facts)
		return
	}
	if got != want {
		t.Errorf("fact %q = %v, want %q", key, got, want)
	}
}

func TestProbeRecordsTheWorkspaceAndBotAsFacts(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	facts := recordedFacts(t, verified)
	assertFact(t, facts, FactWorkspace, "Acme")
	assertFact(t, facts, FactWorkspaceID, "T0ACME")
	assertFact(t, facts, FactBotUser, "opencluster-bot")
	assertFact(t, facts, FactBotUserID, "U0BOT")
}

func TestADegradedProbeStillRecordsWhatItReached(t *testing.T) {
	t.Parallel()

	// A missing scope says nothing about WHICH workspace answered, and the operator on
	// the integration page still has to know that to act on the rest of the note.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded", verified.Status)
	}
	assertFact(t, recordedFacts(t, verified), FactWorkspace, "Acme")
}

func TestAProbeWithUnreadableScopesStillRecordsWhoAnswered(t *testing.T) {
	t.Parallel()

	// Nothing is known about what the token may read, and the identity is still known.
	// The two are separate facts and only one of them failed.
	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("")

	verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
	assertFact(t, recordedFacts(t, verified), FactWorkspace, "Acme")
	assertFact(t, recordedFacts(t, verified), FactBotUser, "opencluster-bot")
}

func TestAFailedProbeEstablishesNothingAboutAWorkspace(t *testing.T) {
	t.Parallel()

	// Facts describe the INSTALLATION, not the attempt. A refused token, a rate limit and
	// an unreachable vendor each establish nothing about a workspace, so each records
	// nothing — and the column keeps whatever the last successful verification put there.
	cases := map[string]func(*fakeSlack){
		"a refused token": func(fake *fakeSlack) {
			fake.answer("auth.test", `{"ok":false,"error":"invalid_auth"}`)
		},
		"a rate limit": func(fake *fakeSlack) {
			fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Retry-After", "1")
				writer.WriteHeader(http.StatusTooManyRequests)
			}
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeSlack(t)
			script(fake)

			verified := probe(testContext(t), NewClient(fake.URL), "xoxb-under-test")
			if verified.Facts != nil {
				t.Errorf("%s recorded facts about a workspace it never reached: %+v",
					name, verified.Facts)
			}
		})
	}

	unreachable := probe(testContext(t), NewClient("http://127.0.0.1:1"), "xoxb-under-test")
	if unreachable.Facts != nil {
		t.Errorf("an unreachable vendor recorded facts: %+v", unreachable.Facts)
	}
}

func TestNoFactCarriesTheCredential(t *testing.T) {
	t.Parallel()

	// Facts are non-secret by construction and no authorization decision reads them. This
	// is the mechanical half of that promise: the plaintext the probe was handed is in
	// scope at the moment the facts are composed, and it must not reach one.
	const token = "xoxb-a-real-looking-secret-value"

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = authTestGranting("channels:read,channels:history,users:read")

	verified := probe(testContext(t), NewClient(fake.URL), token)
	for key, value := range recordedFacts(t, verified) {
		if text, ok := value.(string); ok && strings.Contains(text, token) {
			t.Errorf("fact %q carries the credential", key)
		}
	}
}

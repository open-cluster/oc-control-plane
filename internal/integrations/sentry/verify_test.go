package sentry

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The probe against the fake vendor. What is asserted is the judgement an operator reads:
// a working token is active, a refused token is failed, an unreachable vendor is failed
// without guessing, and a rate limit is degraded rather than failed.

func TestProbeWithAWorkingTokenIsActiveAndNamesTheOrganization(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/", `{"id":"1","slug":"acme","name":"Acme Corp"}`)

	verified := probe(testContext(t), NewClient(fake.URL), "token-under-test", "acme")
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "Acme Corp") {
		t.Errorf("the note %q does not say whose organization answered", verified.Note)
	}
}

func TestProbeWithARevokedTokenIsFailedInTheOperatorsLanguage(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answers["/organizations/acme/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"detail":"Invalid token"}`))
	}

	verified := probe(testContext(t), NewClient(fake.URL), "revoked", "acme")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "Invalid token") {
		t.Errorf("the note %q does not carry the vendor's own reason", verified.Note)
	}
}

func TestProbeWithAnUnknownOrganizationNamesTheSlug(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answers["/organizations/wrong-slug/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"detail":"not found"}`))
	}

	verified := probe(testContext(t), NewClient(fake.URL), "token", "wrong-slug")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "organizationSlug") {
		t.Errorf("the note %q does not point at the field to fix", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), NewClient("http://127.0.0.1:1"), "token", "acme")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeUnderRateLimitingIsDegradedNotFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	reset := strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10)
	fake.answers["/organizations/acme/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Sentry-Rate-Limit-Reset", reset)
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	verified := probe(testContext(t), NewClient(fake.URL), "token", "acme")
	// The vendor answered — it is rate limiting, not refusing the credential — so failed
	// would tell the operator their token died when nothing of the kind is known.
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
}

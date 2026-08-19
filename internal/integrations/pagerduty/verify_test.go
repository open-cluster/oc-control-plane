package pagerduty

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestProbeWithAWorkingTokenIsActive(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents", `{"incidents":[],"more":false}`)

	verified := probe(testContext(t), NewClient(fake.URL), "key-under-test")
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
}

func TestProbeWithARevokedTokenIsFailedInTheOperatorsLanguage(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answers["/incidents"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"Invalid token","code":2006}}`))
	}

	verified := probe(testContext(t), NewClient(fake.URL), "revoked")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "Invalid token") {
		t.Errorf("the note %q does not carry the vendor's own reason", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), NewClient("http://127.0.0.1:1"), "key")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeUnderRateLimitingIsDegradedNotFailed(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answers["/incidents"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	verified := probe(testContext(t), NewClient(fake.URL), "key")
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
}

package newrelic

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestProbeWithAWorkingKeyIsActive(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[],"nextCursor":null}}}}}}`
	}

	verified := probe(testContext(t), NewClient(fake.URL), "us", 123, "key-under-test")
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "us") {
		t.Errorf("the note %q does not name the region", verified.Note)
	}
}

func TestProbeWithARefusedKeyIsFailedInTheOperatorsLanguage(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"errors":[{"message":"Unauthorized"}]}`
	}

	verified := probe(testContext(t), NewClient(fake.URL), "us", 123, "revoked")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "Unauthorized") {
		t.Errorf("the note %q does not carry the vendor's own reason", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), NewClient("http://127.0.0.1:1"), "us", 123, "key")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeUnderRateLimitingIsDegradedNotFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusTooManyRequests, ``
	}

	verified := probe(testContext(t), NewClient(fake.URL), "us", 123, "key")
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
}

package datadog

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func testCredentialJSON(t *testing.T) string {
	t.Helper()
	encoded, err := encodeCredential("api-key-under-test", "app-key-under-test")
	if err != nil {
		t.Fatalf("encodeCredential: %v", err)
	}
	return encoded
}

func TestProbeWithWorkingKeysIsActive(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor", `[]`)

	verified := probe(testContext(t), NewClient(fake.URL), "datadoghq.com", testCredentialJSON(t))
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "datadoghq.com") {
		t.Errorf("the note %q does not name the site", verified.Note)
	}
}

func TestProbeWithAMalformedCredentialIsFailed(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), NewClient(""), "datadoghq.com", "not the json this provider seals")
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
}

func TestProbeWithARefusedKeyPairIsFailedInTheOperatorsLanguage(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"errors":["Forbidden"]}`))
	}

	verified := probe(testContext(t), NewClient(fake.URL), "datadoghq.com", testCredentialJSON(t))
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "api key and a valid application key") {
		t.Errorf("the note %q does not say a read needs both keys", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), NewClient("http://127.0.0.1:1"), "datadoghq.com", testCredentialJSON(t))
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeUnderRateLimitingIsDegradedNotFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	verified := probe(testContext(t), NewClient(fake.URL), "datadoghq.com", testCredentialJSON(t))
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
}

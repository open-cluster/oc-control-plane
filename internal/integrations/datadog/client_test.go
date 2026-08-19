package datadog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client against a fake Datadog. The fake speaks the headers, status codes and error
// envelope the real API does, because the client's whole job is decoding those correctly —
// a test that mocked the client itself would prove nothing about that.

type fakeDatadog struct {
	*httptest.Server
	answers map[string]http.HandlerFunc
}

func newFakeDatadog(t *testing.T) *fakeDatadog {
	t.Helper()

	fake := &fakeDatadog{answers: map[string]http.HandlerFunc{}}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			answer, known := fake.answers[request.URL.Path]
			if !known {
				t.Errorf("the fake was asked for %q and has no answer for it", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			answer(writer, request)
		}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeDatadog) answer(path, body string) {
	f.answers[path] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testCredential(t *testing.T) credential {
	t.Helper()
	return credential{APIKey: "api-key-under-test", ApplicationKey: "app-key-under-test"}
}

func TestMonitorsSendsBothKeys(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	var seenAPIKey, seenAppKey string
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, request *http.Request) {
		seenAPIKey = request.Header.Get("DD-API-KEY")
		seenAppKey = request.Header.Get("DD-APPLICATION-KEY")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	_, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t), MonitorsQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if seenAPIKey != "api-key-under-test" || seenAppKey != "app-key-under-test" {
		t.Errorf("api key = %q, app key = %q; both must reach the vendor", seenAPIKey, seenAppKey)
	}
}

func TestMonitorsReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor", `[
		{"id":1,"name":"checkout latency","type":"metric alert","overall_state":"Alert"},
		{"id":2,"name":"checkout errors","type":"metric alert","overall_state":"OK"}
	]`)

	monitors, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t), MonitorsQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if len(monitors.Monitors) != 2 || monitors.Monitors[0].ID != 1 {
		t.Errorf("monitors = %+v", monitors.Monitors)
	}
	if monitors.Truncated {
		t.Error("truncated = true, want false: a page shorter than the limit is the whole answer")
	}
}

func TestMonitorsFlagsTruncationWhenAPageIsFull(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor", `[{"id":1,"name":"a"},{"id":2,"name":"b"}]`)

	monitors, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t), MonitorsQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if !monitors.Truncated {
		t.Error("truncated = false, want true: the page came back exactly at the limit")
	}
}

func TestMonitorsQueryParametersReachTheVendor(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	var seenName, seenTags, seenSize string
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, request *http.Request) {
		seenName = request.URL.Query().Get("name")
		seenTags = request.URL.Query().Get("monitor_tags")
		seenSize = request.URL.Query().Get("page_size")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	_, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t),
		MonitorsQuery{Name: "checkout", Tags: "env:prod", Limit: 10})
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if seenName != "checkout" || seenTags != "env:prod" || seenSize != "10" {
		t.Errorf("name=%q tags=%q page_size=%q", seenName, seenTags, seenSize)
	}
}

func TestMonitorRetrievesOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answer("/api/v1/monitor/42", `{"id":42,"name":"checkout latency","overall_state":"Alert","query":"avg(last_5m):avg:trace.http.request.duration{service:checkout} > 1"}`)

	monitor, err := NewClient(fake.URL).Monitor(testContext(t), "datadoghq.com", testCredential(t), 42)
	if err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	if monitor.Name != "checkout latency" || monitor.OverallState != "Alert" {
		t.Errorf("monitor = %+v", monitor)
	}
}

func TestARefusalDecodesTheErrorsEnvelope(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"errors":["Forbidden"]}`))
	}

	_, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t), MonitorsQuery{Limit: 25})
	refusal, isAPIError := err.(*APIError)
	if !isAPIError {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if refusal.Status != http.StatusForbidden || refusal.Detail != "Forbidden" {
		t.Errorf("refusal = %+v", refusal)
	}
}

func TestRateLimitedIsErrRateLimited(t *testing.T) {
	t.Parallel()

	fake := newFakeDatadog(t)
	fake.answers["/api/v1/monitor"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	_, err := NewClient(fake.URL).Monitors(testContext(t), "datadoghq.com", testCredential(t), MonitorsQuery{Limit: 25})
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestSiteChoosesTheOriginWhenNoOverrideIsSet(t *testing.T) {
	t.Parallel()

	client := NewClient("")
	if got := client.origin("datadoghq.eu"); got != "https://api.datadoghq.eu" {
		t.Errorf("origin = %q", got)
	}
	if got := client.origin("us3.datadoghq.com"); got != "https://api.us3.datadoghq.com" {
		t.Errorf("origin = %q", got)
	}
}

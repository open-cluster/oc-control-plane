package pagerduty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client against a fake PagerDuty. The fake speaks the headers, status codes and
// error envelope the real API does, because the client's whole job is decoding those
// correctly — a test that mocked the client itself would prove nothing about that.

type fakePagerDuty struct {
	*httptest.Server
	answers map[string]http.HandlerFunc
}

func newFakePagerDuty(t *testing.T) *fakePagerDuty {
	t.Helper()

	fake := &fakePagerDuty{answers: map[string]http.HandlerFunc{}}
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

func (f *fakePagerDuty) answer(path, body string) {
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

func TestIncidentsSendsTheTokenAndVersionHeader(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	var seenAuth, seenAccept string
	fake.answers["/incidents"] = func(writer http.ResponseWriter, request *http.Request) {
		seenAuth = request.Header.Get("Authorization")
		seenAccept = request.Header.Get("Accept")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"incidents":[],"more":false}`))
	}

	if _, err := NewClient(fake.URL).Incidents(testContext(t), "key-under-test", IncidentsQuery{Limit: 25}); err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if seenAuth != "Token token=key-under-test" {
		t.Errorf("Authorization = %q", seenAuth)
	}
	if seenAccept != apiVersion {
		t.Errorf("Accept = %q, want %q", seenAccept, apiVersion)
	}
}

func TestIncidentsReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents", `{"incidents":[
		{"id":"PT4KHLK","incident_number":1234,"title":"The server is on fire.","status":"triggered","urgency":"high","service":{"summary":"My Mail Service"}}
	],"more":false}`)

	incidents, err := NewClient(fake.URL).Incidents(testContext(t), "key", IncidentsQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if len(incidents.Incidents) != 1 || incidents.Incidents[0].ID != "PT4KHLK" {
		t.Errorf("incidents = %+v", incidents.Incidents)
	}
	if incidents.Incidents[0].ServiceName != "My Mail Service" {
		t.Errorf("service name = %q; the nested service reference must be flattened", incidents.Incidents[0].ServiceName)
	}
	if incidents.Truncated {
		t.Error("truncated = true, want false: the vendor's own \"more\" was false")
	}
}

func TestIncidentsFlagsTruncationFromMore(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents", `{"incidents":[],"more":true}`)

	incidents, err := NewClient(fake.URL).Incidents(testContext(t), "key", IncidentsQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if !incidents.Truncated {
		t.Error("truncated = false, want true: the vendor's own \"more\" was true")
	}
}

func TestIncidentsQueryParametersReachTheVendor(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	var seenStatuses, seenUrgencies []string
	var seenLimit string
	fake.answers["/incidents"] = func(writer http.ResponseWriter, request *http.Request) {
		seenStatuses = request.URL.Query()["statuses[]"]
		seenUrgencies = request.URL.Query()["urgencies[]"]
		seenLimit = request.URL.Query().Get("limit")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"incidents":[],"more":false}`))
	}

	_, err := NewClient(fake.URL).Incidents(testContext(t), "key", IncidentsQuery{
		Statuses: []string{"triggered", "acknowledged"}, Urgencies: []string{"high"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if len(seenStatuses) != 2 || seenStatuses[0] != "triggered" || seenStatuses[1] != "acknowledged" {
		t.Errorf("statuses[] = %v", seenStatuses)
	}
	if len(seenUrgencies) != 1 || seenUrgencies[0] != "high" {
		t.Errorf("urgencies[] = %v", seenUrgencies)
	}
	if seenLimit != "10" {
		t.Errorf("limit = %q", seenLimit)
	}
}

func TestIncidentRetrievesOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answer("/incidents/PT4KHLK",
		`{"incident":{"id":"PT4KHLK","incident_number":1234,"title":"The server is on fire.","status":"acknowledged","urgency":"high"}}`)

	incident, err := NewClient(fake.URL).Incident(testContext(t), "key", "PT4KHLK")
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if incident.Title != "The server is on fire." || incident.Status != "acknowledged" {
		t.Errorf("incident = %+v", incident)
	}
}

func TestARefusalDecodesTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answers["/incidents"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"Invalid token","code":2006}}`))
	}

	_, err := NewClient(fake.URL).Incidents(testContext(t), "key", IncidentsQuery{Limit: 25})
	refusal, isAPIError := err.(*APIError)
	if !isAPIError {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if refusal.Status != http.StatusUnauthorized || refusal.Message != "Invalid token" {
		t.Errorf("refusal = %+v", refusal)
	}
}

func TestRateLimitedIsErrRateLimited(t *testing.T) {
	t.Parallel()

	fake := newFakePagerDuty(t)
	fake.answers["/incidents"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	_, err := NewClient(fake.URL).Incidents(testContext(t), "key", IncidentsQuery{Limit: 25})
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

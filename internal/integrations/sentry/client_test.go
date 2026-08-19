package sentry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// The client against a fake Sentry. The fake speaks the status codes, headers and error
// shapes the real API does, because the client's whole job is decoding those correctly —
// a test that mocked the client itself would prove nothing about that.

// fakeSentry is a configurable stand-in for the Sentry API.
type fakeSentry struct {
	*httptest.Server
	// answers maps a request path to what the fake returns for it.
	answers map[string]http.HandlerFunc
}

func newFakeSentry(t *testing.T) *fakeSentry {
	t.Helper()

	fake := &fakeSentry{answers: map[string]http.HandlerFunc{}}
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

// answer sets what one path returns as a plain 200 JSON body.
func (f *fakeSentry) answer(path, body string) {
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

func TestOrganizationReturnsIdentity(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/", `{"id":"1","slug":"acme","name":"Acme Corp"}`)

	organization, err := NewClient(fake.URL).Organization(testContext(t), "token-under-test", "acme")
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	if organization.Slug != "acme" || organization.Name != "Acme Corp" {
		t.Errorf("organization = %+v", organization)
	}
}

func TestOrganizationSendsBearerAuthorization(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	var seen string
	fake.answers["/organizations/acme/"] = func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"1","slug":"acme","name":"Acme"}`))
	}

	if _, err := NewClient(fake.URL).Organization(testContext(t), "secret-token", "acme"); err != nil {
		t.Fatalf("Organization: %v", err)
	}
	if seen != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want a bearer token", seen)
	}
}

func TestOrganizationARefusalIsAnAPIError(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answers["/organizations/acme/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"detail":"Invalid token"}`))
	}

	_, err := NewClient(fake.URL).Organization(testContext(t), "revoked", "acme")
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if refusal.Status != http.StatusUnauthorized || refusal.Detail != "Invalid token" {
		t.Errorf("refusal = %+v", refusal)
	}
}

func TestIssuesDecodesCountAsStringOrNumber(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/issues/", `[
		{"id":"1","shortId":"ACME-1","title":"NPE","count":"42","userCount":"7"},
		{"id":"2","shortId":"ACME-2","title":"timeout","count":13,"userCount":2}
	]`)

	issues, err := NewClient(fake.URL).Issues(testContext(t), "token", "acme", IssuesQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(issues.Issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues.Issues))
	}
	if issues.Issues[0].Count != "42" || issues.Issues[1].Count != "13" {
		t.Errorf("counts = %q, %q; both string and number shapes must decode",
			issues.Issues[0].Count, issues.Issues[1].Count)
	}
}

func TestIssuesFlagsTruncationFromTheLinkHeader(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answers["/organizations/acme/issues/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Link",
			`<https://sentry.io/x>; rel="previous"; results="false", `+
				`<https://sentry.io/y>; rel="next"; results="true"`)
		_, _ = writer.Write([]byte(`[{"id":"1","shortId":"ACME-1","title":"NPE"}]`))
	}

	issues, err := NewClient(fake.URL).Issues(testContext(t), "token", "acme", IssuesQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if !issues.Truncated {
		t.Error("truncated = false, want true: the Link header names a next page with results")
	}
}

func TestIssuesQueryParametersReachTheVendor(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	var seenQuery, seenPeriod, seenLimit string
	fake.answers["/organizations/acme/issues/"] = func(writer http.ResponseWriter, request *http.Request) {
		seenQuery = request.URL.Query().Get("query")
		seenPeriod = request.URL.Query().Get("statsPeriod")
		seenLimit = request.URL.Query().Get("limit")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	_, err := NewClient(fake.URL).Issues(testContext(t), "token", "acme",
		IssuesQuery{Query: "is:unresolved", StatsPeriod: "24h", Limit: 10})
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if seenQuery != "is:unresolved" || seenPeriod != "24h" || seenLimit != "10" {
		t.Errorf("query=%q statsPeriod=%q limit=%q", seenQuery, seenPeriod, seenLimit)
	}
}

func TestIssueRetrievesOneWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	fake.answer("/organizations/acme/issues/123/",
		`{"id":"123","shortId":"ACME-9","title":"panic","culprit":"handler.go","level":"error","status":"unresolved"}`)

	issue, err := NewClient(fake.URL).Issue(testContext(t), "token", "acme", "123")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.ShortID != "ACME-9" || issue.Culprit != "handler.go" {
		t.Errorf("issue = %+v", issue)
	}
}

func TestRateLimitedTwiceIsErrRateLimited(t *testing.T) {
	t.Parallel()

	fake := newFakeSentry(t)
	reset := strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10)
	fake.answers["/organizations/acme/"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Sentry-Rate-Limit-Reset", reset)
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	_, err := NewClient(fake.URL).Organization(testContext(t), "token", "acme")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestUnreachableVendorIsAPlainTransportError(t *testing.T) {
	t.Parallel()

	// A closed port: nothing answers at all, which is a different fact from a refusal.
	_, err := NewClient("http://127.0.0.1:1").Organization(testContext(t), "token", "acme")
	if err == nil {
		t.Fatal("err = nil, want a transport error")
	}
	var refusal *APIError
	if errors.As(err, &refusal) {
		t.Errorf("err = %v is an *APIError; an unreachable vendor never answered one", err)
	}
}

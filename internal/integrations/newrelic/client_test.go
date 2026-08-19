package newrelic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client against a fake NerdGraph. The fake decodes the GraphQL request body and
// answers with the envelope shape the real API uses, because the client's whole job is
// speaking that envelope correctly.

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type fakeNerdGraph struct {
	*httptest.Server
	answer func(graphqlRequest) (int, string)
	// lastRequest captures what the client actually sent, for tests that assert on it.
	lastRequest graphqlRequest
	lastHeaders http.Header
}

func newFakeNerdGraph(t *testing.T) *fakeNerdGraph {
	t.Helper()

	fake := &fakeNerdGraph{}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			var decoded graphqlRequest
			if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
				t.Errorf("the fake could not decode the request body: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			fake.lastRequest = decoded
			fake.lastHeaders = request.Header.Clone()

			if fake.answer == nil {
				t.Error("the fake has no answer configured")
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			status, body := fake.answer(decoded)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(body))
		}))
	t.Cleanup(fake.Close)
	return fake
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestIssuesSendsTheExperimentalOptInAndTheKey(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[],"nextCursor":null}}}}}}`
	}

	if _, err := NewClient(fake.URL).Issues(testContext(t), "us", "key-under-test", 123, IssuesQuery{Limit: 25}); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if fake.lastHeaders.Get("API-Key") != "key-under-test" {
		t.Errorf("API-Key header = %q", fake.lastHeaders.Get("API-Key"))
	}
	if fake.lastHeaders.Get(experimentalOptInHeader) != experimentalOptInValue {
		t.Errorf("opt-in header = %q, want %q", fake.lastHeaders.Get(experimentalOptInHeader), experimentalOptInValue)
	}
	if id, _ := fake.lastRequest.Variables["accountId"].(float64); int(id) != 123 {
		t.Errorf("accountId variable = %v", fake.lastRequest.Variables["accountId"])
	}
}

func TestIssuesReturnsWhatTheVendorAnswers(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[
			{"issueId":"1","title":"checkout down","priority":"CRITICAL","state":"ACTIVATED"},
			{"issueId":"2","title":"latency","priority":"HIGH","state":"ACTIVATED"}
		],"nextCursor":null}}}}}}`
	}

	issues, err := NewClient(fake.URL).Issues(testContext(t), "us", "key", 123, IssuesQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(issues.Issues) != 2 || issues.Issues[0].ID != "1" {
		t.Errorf("issues = %+v", issues.Issues)
	}
	if issues.Truncated {
		t.Error("truncated = true, want false: nextCursor was null")
	}
}

func TestIssuesFlagsTruncationFromNextCursor(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[
			{"issueId":"1","title":"a"}
		],"nextCursor":"abc123"}}}}}}`
	}

	issues, err := NewClient(fake.URL).Issues(testContext(t), "us", "key", 123, IssuesQuery{Limit: 25})
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if !issues.Truncated {
		t.Error("truncated = false, want true: nextCursor was non-empty")
	}
}

func TestIssueReturnsNotFoundForAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"data":{"actor":{"account":{"aiIssues":{"issues":{"issues":[]}}}}}}`
	}

	_, err := NewClient(fake.URL).Issue(testContext(t), "us", "key", 123, "missing")
	refusal, isAPIError := err.(*APIError)
	if !isAPIError || refusal.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a not-found *APIError", err)
	}
}

func TestGraphQLErrorsArrayIsARefusalEvenOnHTTP200(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusOK, `{"errors":[{"message":"Unauthorized"}]}`
	}

	_, err := NewClient(fake.URL).Issues(testContext(t), "us", "bad-key", 123, IssuesQuery{Limit: 25})
	refusal, isAPIError := err.(*APIError)
	if !isAPIError {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if refusal.Detail != "Unauthorized" {
		t.Errorf("detail = %q", refusal.Detail)
	}
}

func TestRateLimitedIsErrRateLimited(t *testing.T) {
	t.Parallel()

	fake := newFakeNerdGraph(t)
	fake.answer = func(graphqlRequest) (int, string) {
		return http.StatusTooManyRequests, ``
	}

	_, err := NewClient(fake.URL).Issues(testContext(t), "us", "key", 123, IssuesQuery{Limit: 25})
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestOriginResolvesTheThreeRegions(t *testing.T) {
	t.Parallel()

	client := NewClient("")
	if got := client.origin("us"); got != "https://api.newrelic.com/graphql" {
		t.Errorf("us origin = %q", got)
	}
	if got := client.origin("eu"); got != "https://api.eu.newrelic.com/graphql" {
		t.Errorf("eu origin = %q", got)
	}
	if got := client.origin("mars"); got != "" {
		t.Errorf("unknown region origin = %q, want empty", got)
	}
}

package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/anthropic"
)

// THE SEAM IS THE HTTP ROUND-TRIPPER, AND THIS SUITE NEVER REACHES THE NETWORK.
//
// Every case here is a canned response. A test that called the real API would be non-deterministic,
// priced and offline-hostile — three properties a commit gate must not have. What is asserted is
// what this adapter SENDS and what it does with what comes back, never whether an answer was any
// good.

// transport is the canned round-tripper.
type transport struct {
	mutex     sync.Mutex
	responses []*http.Response
	requests  []*http.Request
	bodies    []string
	calls     int
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	body := ""
	if request.Body != nil {
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
	}
	t.requests = append(t.requests, request)
	t.bodies = append(t.bodies, body)

	index := t.calls
	t.calls++
	if index >= len(t.responses) {
		index = len(t.responses) - 1
	}
	return t.responses[index], nil
}

func (t *transport) callCount() int {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.calls
}

func (t *transport) lastBody(tb testing.TB) string {
	tb.Helper()
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if len(t.bodies) == 0 {
		tb.Fatal("nothing was sent")
	}
	return t.bodies[len(t.bodies)-1]
}

// streamed builds a successful streaming response carrying one text document.
func streamed(document string, usage string, stopReason string, stopDetails string) *http.Response {
	events := &strings.Builder{}
	fmt.Fprintf(events, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":"+
		"{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\","+
		"\"model\":\"claude-opus-5\",\"content\":[],\"stop_reason\":null,"+
		"\"stop_sequence\":null,\"usage\":%s}}\n\n", usage)
	events.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\"," +
		"\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	if document != "" {
		fmt.Fprintf(events, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\","+
			"\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n",
			quoted(document))
	}
	events.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"," +
		"\"index\":0}\n\n")

	details := ""
	if stopDetails != "" {
		details = ",\"stop_details\":" + stopDetails
	}
	fmt.Fprintf(events, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":"+
		"{\"stop_reason\":\"%s\",\"stop_sequence\":null%s},\"usage\":%s}\n\n",
		stopReason, details, usage)
	events.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(events.String())),
	}
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("request-id", "req_from_provider")
	return response
}

func failedWith(status int, body string) *http.Response {
	response := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("request-id", "req_from_provider")
	return response
}

func quoted(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	return `"` + escaped + `"`
}

const fullUsage = `{"input_tokens":1000,"output_tokens":250,` +
	`"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,` +
	`"output_tokens_details":{"thinking_tokens":120}}`

func providerUnder(t *testing.T, responses ...*http.Response) (*anthropic.Provider, *transport) {
	t.Helper()
	round := &transport{responses: responses}
	provider, err := anthropic.New(reasoning.Deployment{
		Provider:        anthropic.Name,
		Model:           "claude-opus-5",
		Effort:          reasoning.EffortHigh,
		Credential:      reasoning.Secret("sk-test-credential"),
		MaxOutputTokens: 32_000,
		MaxAttempts:     2,
		RequestTimeout:  5 * time.Second,
	}, anthropic.Options{HTTPClient: &http.Client{Transport: round}})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider, round
}

func promptFixture() reasoning.Prompt {
	return reasoning.Prompt{
		Model:   "claude-opus-5",
		System:  []reasoning.Block{{Text: "the frozen preamble", Cache: true}},
		Content: []reasoning.Block{{Text: "the brief", Cache: true}, {Text: "the task"}},
		Schema: reasoning.Schema{
			Name:    "judgement",
			Version: "1",
			Document: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"findings": map[string]any{"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"statement": map[string]any{"type": "string"},
							},
							"required":             []any{"statement"},
							"additionalProperties": false,
						}},
				},
				"required":             []any{"findings"},
				"additionalProperties": false,
			},
		},
		MaxOutputTokens: 32_000,
		Effort:          reasoning.EffortHigh,
	}
}

func TestComplete_ReturnsTheDocumentAndNormalizesUsage(t *testing.T) {
	provider, _ := providerUnder(t, streamed(`{"findings":[]}`, fullUsage, "end_turn", ""))

	completion, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}

	if string(completion.Document) != `{"findings":[]}` {
		t.Errorf("the document is %q", completion.Document)
	}
	// The model that ANSWERED, read from the response rather than echoed from the request.
	if completion.Model != "claude-opus-5" {
		t.Errorf("the answering model is %q", completion.Model)
	}
	if completion.RequestID != "req_from_provider" {
		t.Errorf("the provider's request identifier is %q", completion.RequestID)
	}
	if completion.Stop != reasoning.StopComplete {
		t.Errorf("stop is %s, want complete", completion.Stop)
	}

	usage := completion.Usage
	if usage.Input.Or(0) != 1000 || usage.Output.Or(0) != 250 {
		t.Errorf("input and output tokens are wrong: %+v", usage)
	}
	// Both cache figures are recorded, which is what makes cache effectiveness measurable rather
	// than assumed: a cache that silently stopped working looks exactly like one that is working
	// unless both are there.
	if usage.CacheWrite.Or(0) != 300 || usage.CacheRead.Or(0) != 4000 {
		t.Errorf("cache tokens are wrong: %+v", usage)
	}
	if !usage.Reasoning.Reported || usage.Reasoning.Tokens != 120 {
		t.Errorf("reasoning tokens are wrong: %+v", usage.Reasoning)
	}
	// Reasoning tokens sit INSIDE the output total on this provider. Adding them would bill every
	// round that thought twice in this system's own figures.
	if usage.Billable() != 1000+250+300+4000 {
		t.Errorf("billable tokens are %d, want reasoning tokens not added on top",
			usage.Billable())
	}
}

func TestComplete_AReasoningFigureTheProviderDidNotReportIsAbsentRatherThanZero(t *testing.T) {
	usage := `{"input_tokens":10,"output_tokens":5,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
	provider, _ := providerUnder(t, streamed(`{}`, usage, "end_turn", ""))

	completion, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}
	if completion.Usage.Reasoning.Reported {
		t.Error("a reasoning figure the provider never reported is recorded as measured")
	}
}

func TestComplete_ARefusalIsANamedFailureAndNeverADocument(t *testing.T) {
	details := `{"type":"refusal","category":"cyber","explanation":"declined"}`
	provider, _ := providerUnder(t,
		streamed(`{"findings":[]}`, fullUsage, "refusal", details))

	completion, err := provider.Complete(context.Background(), promptFixture())

	if !errors.Is(err, reasoning.ErrRefused) {
		t.Fatalf("got %v, want a named refusal", err)
	}
	// Asserted before the content is read, because reading first is the defect that presents an
	// empty or partial response as a conclusion.
	if len(completion.Document) != 0 {
		t.Errorf("a refused request returned a document: %q", completion.Document)
	}
	if completion.Stop != reasoning.StopRefused {
		t.Errorf("stop is %s, want refused", completion.Stop)
	}
	// The completion is still populated, because a refused request consumed real tokens and the
	// record has to carry them.
	if completion.Usage.Input.Or(0) != 1000 {
		t.Errorf("a refused request recorded %d input tokens, want 1000",
			completion.Usage.Input.Or(0))
	}
	if completion.RequestID == "" {
		t.Error("a refused request recorded no provider request identifier")
	}

	var failure *reasoning.Failure
	if errors.As(err, &failure) && failure.Category != "cyber" {
		t.Errorf("the refusal category is %q, want the provider's own", failure.Category)
	}
	// A refusal is a fact about the provider. It must never read as an abstention, which is a
	// finding about the evidence.
	if errors.Is(err, reasoning.ErrMalformed) || errors.Is(err, reasoning.ErrOutage) {
		t.Error("a refusal also reads as another outcome")
	}
}

func TestComplete_TellsARejectedRequestApartFromAnOutage(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"a malformed request is this build's defect", 400, reasoning.ErrRejected},
		{"a rejected credential is this deployment's problem", 401, reasoning.ErrRejected},
		{"an unknown model is a configuration mistake", 404, reasoning.ErrRejected},
		{"a server error is the vendor's", 500, reasoning.ErrOutage},
		{"an overloaded provider is the vendor's", 529, reasoning.ErrOutage},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider, _ := providerUnder(t, failedWith(testCase.status,
				`{"type":"error","error":{"type":"x","message":"y"}}`))

			_, err := provider.Complete(context.Background(), promptFixture())
			if !errors.Is(err, testCase.want) {
				t.Fatalf("a %d became %v, want %v", testCase.status, err, testCase.want)
			}
		})
	}
}

func TestComplete_RateLimitingFollowedBySuccessReturnsTheAnswer(t *testing.T) {
	provider, round := providerUnder(t,
		failedWith(429, `{"type":"error","error":{"type":"rate_limit_error"}}`),
		streamed(`{"findings":[]}`, fullUsage, "end_turn", ""))

	completion, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("a retried rate limit did not recover: %v", err)
	}
	if string(completion.Document) != `{"findings":[]}` {
		t.Errorf("the document is %q", completion.Document)
	}
	if round.callCount() < 2 {
		t.Errorf("the provider was called %d times, want the rate limit to have been retried",
			round.callCount())
	}
}

func TestComplete_RateLimitingThroughoutBecomesAnOutage(t *testing.T) {
	provider, _ := providerUnder(t,
		failedWith(429, `{"type":"error","error":{"type":"rate_limit_error"}}`))

	_, err := provider.Complete(context.Background(), promptFixture())
	if !errors.Is(err, reasoning.ErrOutage) {
		t.Fatalf("got %v, want rate limiting past the retry budget to be an outage", err)
	}
}

func TestComplete_ATruncatedAnswerIsRefusedRatherThanParsed(t *testing.T) {
	provider, _ := providerUnder(t,
		streamed(`{"hypotheses":[{"statement":"half a doc`, fullUsage, "max_tokens", ""))

	completion, err := provider.Complete(context.Background(), promptFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a truncated answer to be refused", err)
	}
	if completion.Stop != reasoning.StopTruncated {
		t.Errorf("stop is %s, want truncated", completion.Stop)
	}
	if len(completion.Document) != 0 {
		t.Error("a truncated answer returned a document that cannot be trusted to parse")
	}
}

func TestComplete_ACancelledContextStopsTheCallAndReturnsPromptly(t *testing.T) {
	provider, _ := providerUnder(t, streamed(`{}`, fullUsage, "end_turn", ""))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := provider.Complete(ctx, promptFixture())
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, reasoning.ErrTimeout) {
			t.Fatalf("got %v, want a cancelled context to end the call as a timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled context did not stop the call")
	}
}

func TestComplete_SendsTheDeclaredSchemaEffortAndCacheBreakpoints(t *testing.T) {
	provider, round := providerUnder(t, streamed(`{}`, fullUsage, "end_turn", ""))

	if _, err := provider.Complete(context.Background(), promptFixture()); err != nil {
		t.Fatalf("completing: %v", err)
	}
	body := round.lastBody(t)

	for _, expected := range []string{
		`"output_config"`,    // the schema and the effort travel together
		`"effort":"high"`,    // effort is configuration, not a constant
		`"json_schema"`,      // the answer is constrained rather than parsed out of prose
		`"adaptive"`,         // thinking depth is set by effort, not a token budget
		`"cache_control"`,    // the breakpoints the prompt asked for
		`"max_tokens":32000`, // a generous ceiling, because thinking shares it
		`"stream":true`,      // which is why the request streams
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("the request does not carry %s", expected)
		}
	}
	// Sampling parameters are removed on this model and sending one is refused outright.
	for _, forbidden := range []string{`"temperature"`, `"top_p"`, `"top_k"`, `"budget_tokens"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the request carries %s, which this model rejects", forbidden)
		}
	}
}

func TestComplete_TheCredentialAppearsInNoErrorReturnedToTheCaller(t *testing.T) {
	provider, _ := providerUnder(t, failedWith(401, `{"type":"error","error":{"message":"no"}}`))

	_, err := provider.Complete(context.Background(), promptFixture())
	if err == nil {
		t.Fatal("a rejected credential did not fail")
	}
	if strings.Contains(err.Error(), "sk-test-credential") {
		t.Error("the credential reached an error message the caller will log")
	}
}

func TestNew_RefusesADeploymentThatCouldNotWork(t *testing.T) {
	cases := map[string]reasoning.Deployment{
		"no model":      {Provider: anthropic.Name, Credential: reasoning.Secret("k")},
		"no credential": {Provider: anthropic.Name, Model: "claude-opus-5"},
		"an effort level that does not exist": {
			Provider: anthropic.Name, Model: "claude-opus-5",
			Credential: reasoning.Secret("k"), Effort: reasoning.Effort("enormous"),
		},
		"a base url that is not https": {
			Provider: anthropic.Name, Model: "claude-opus-5",
			Credential: reasoning.Secret("k"), BaseURL: "http://insecure.example",
		},
	}
	for name, deployment := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := anthropic.New(deployment, anthropic.Options{}); err == nil {
				t.Error("a deployment that could not work was accepted at startup")
			}
		})
	}
}

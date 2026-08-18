package zai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/zai"
)

// THE SEAM IS THE HTTP ROUND-TRIPPER, AND THIS SUITE NEVER REACHES THE NETWORK.
//
// What is worth asserting here is mostly where this vendor DIFFERS from the other one: a JSON
// mode rather than schema enforcement, cached input tokens with no cache-write count, and its
// own word for a refusal.

type transport struct {
	mutex     sync.Mutex
	responses []*http.Response
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
	t.bodies = append(t.bodies, body)

	index := t.calls
	t.calls++
	if index >= len(t.responses) {
		index = len(t.responses) - 1
	}
	return t.responses[index], nil
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

func answered(status int, body string) *http.Response {
	response := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("X-Request-Id", "req_from_provider")
	return response
}

// completion is this vendor's answer envelope.
func completion(content, finishReason, usage string) string {
	return `{"id":"chat_test","request_id":"zai_req_1","model":"glm-4.7",` +
		`"choices":[{"index":0,"finish_reason":"` + finishReason + `",` +
		`"message":{"role":"assistant","content":` + quoted(content) +
		`,"reasoning_content":"internal reasoning that is not the document"}}],` +
		`"usage":` + usage + `}`
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

const cachedUsage = `{"prompt_tokens":5000,"completion_tokens":300,"total_tokens":5300,` +
	`"prompt_tokens_details":{"cached_tokens":4000}}`

func providerUnder(t *testing.T, responses ...*http.Response) (*zai.Provider, *transport) {
	t.Helper()
	round := &transport{responses: responses}
	provider, err := zai.New(reasoning.Deployment{
		Provider:        zai.Name,
		Model:           "glm-4.7",
		Effort:          reasoning.EffortHigh,
		Credential:      reasoning.Secret("zai-test-credential"),
		MaxOutputTokens: 32_000,
		RequestTimeout:  5 * time.Second,
	}, zai.Options{HTTPClient: &http.Client{Transport: round}})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider, round
}

func promptFixture() reasoning.Prompt {
	return reasoning.Prompt{
		Model:           "glm-4.7",
		System:          []reasoning.Block{{Text: "the frozen preamble", Cache: true}},
		Content:         []reasoning.Block{{Text: "the brief", Cache: true}, {Text: "the task"}},
		Schema:          reasoning.ConclusionSchema(),
		MaxOutputTokens: 32_000,
		Effort:          reasoning.EffortHigh,
	}
}

func TestComplete_ReturnsTheDocumentAndTheModelThatAnswered(t *testing.T) {
	provider, _ := providerUnder(t,
		answered(200, completion(`{"hypotheses":[]}`, "stop", cachedUsage)))

	answer, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}

	if string(answer.Document) != `{"hypotheses":[]}` {
		t.Errorf("the document is %q", answer.Document)
	}
	if answer.Model != "glm-4.7" {
		t.Errorf("the answering model is %q", answer.Model)
	}
	if answer.RequestID != "zai_req_1" {
		t.Errorf("the provider's request identifier is %q", answer.RequestID)
	}
	if answer.Stop != reasoning.StopComplete {
		t.Errorf("stop is %s, want complete", answer.Stop)
	}
}

func TestComplete_TakesCachedTokensOutOfTheInputTotalAndReportsNoCacheWrite(t *testing.T) {
	provider, _ := providerUnder(t,
		answered(200, completion(`{"hypotheses":[]}`, "stop", cachedUsage)))

	answer, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}
	usage := answer.Usage

	// Cached tokens are part of the prompt total on this vendor. Leaving them in would charge the
	// cached portion at the full input rate, which is the exact mistake four-rate pricing exists
	// to prevent.
	if usage.Input.Or(0) != 1000 {
		t.Errorf("input tokens are %d, want the prompt total minus the cached part",
			usage.Input.Or(0))
	}
	if usage.CacheRead.Or(0) != 4000 {
		t.Errorf("cache-read tokens are %d, want 4000", usage.CacheRead.Or(0))
	}
	// This vendor says nothing at all about tokens written to a cache. Absent is not zero: a zero
	// would claim a measurement nobody made, and would make a cache that stopped working
	// indistinguishable from a vendor that never reported one.
	if usage.CacheWrite.Reported {
		t.Error("a cache-write figure this vendor never reports is recorded as measured")
	}
	if usage.Billable() != 1000+300+4000 {
		t.Errorf("billable tokens are %d", usage.Billable())
	}
}

func TestComplete_ACachedFigureLargerThanTheTotalDoesNotProduceNegativeInput(t *testing.T) {
	usage := `{"prompt_tokens":100,"completion_tokens":10,` +
		`"prompt_tokens_details":{"cached_tokens":900}}`
	provider, _ := providerUnder(t, answered(200, completion(`{}`, "stop", usage)))

	answer, err := provider.Complete(context.Background(), promptFixture())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}
	if answer.Usage.Input.Or(0) < 0 {
		t.Errorf("input tokens are %d, which would cost a round less than nothing",
			answer.Usage.Input.Or(0))
	}
}

func TestComplete_ThisVendorsWordForARefusalIsANamedRefusal(t *testing.T) {
	provider, _ := providerUnder(t, answered(200, completion("", "sensitive", cachedUsage)))

	answer, err := provider.Complete(context.Background(), promptFixture())

	if !errors.Is(err, reasoning.ErrRefused) {
		t.Fatalf("got %v, want this vendor's refusal to be a named refusal", err)
	}
	if answer.Stop != reasoning.StopRefused {
		t.Errorf("stop is %s, want refused", answer.Stop)
	}
	if len(answer.Document) != 0 {
		t.Error("a refused request returned a document")
	}
	// Still populated, because a refused request consumed real tokens.
	if answer.Usage.Input.Or(0) == 0 {
		t.Error("a refused request recorded no input tokens")
	}
	// A refusal is a fact about the provider and must never read as an abstention, which is a
	// finding about the evidence.
	if errors.Is(err, reasoning.ErrOutage) || errors.Is(err, reasoning.ErrRejected) {
		t.Error("a refusal also reads as another outcome")
	}
}

func TestComplete_ATruncatedAnswerIsRefusedRatherThanParsed(t *testing.T) {
	provider, _ := providerUnder(t,
		answered(200, completion(`{"hypotheses":[{"statem`, "length", cachedUsage)))

	answer, err := provider.Complete(context.Background(), promptFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a truncated answer to be refused", err)
	}
	if answer.Stop != reasoning.StopTruncated {
		t.Errorf("stop is %s, want truncated", answer.Stop)
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
		{"rate limiting is the vendor's", 429, reasoning.ErrOutage},
		{"a server error is the vendor's", 500, reasoning.ErrOutage},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider, _ := providerUnder(t, answered(testCase.status,
				`{"error":{"code":"1002","message":"something the vendor said"}}`))

			_, err := provider.Complete(context.Background(), promptFixture())
			if !errors.Is(err, testCase.want) {
				t.Fatalf("a %d became %v, want %v", testCase.status, err, testCase.want)
			}
		})
	}
}

func TestComplete_RendersTheSchemaIntoThePromptBecauseThisVendorCannotEnforceIt(t *testing.T) {
	provider, round := providerUnder(t,
		answered(200, completion(`{"findings":[]}`, "stop", cachedUsage)))

	if _, err := provider.Complete(context.Background(), promptFixture()); err != nil {
		t.Fatalf("completing: %v", err)
	}
	body := round.lastBody(t)

	// The workaround for a missing capability belongs to the provider that is missing it, so a
	// vendor that enforces schemas natively is not charged for one that does not. The marker is
	// a property only the declared schema carries.
	if !strings.Contains(body, `\"statement\"`) && !strings.Contains(body, `"statement"`) {
		t.Error("the schema was not rendered into the prompt, so this vendor has nothing to " +
			"match the answer against")
	}
	if !strings.Contains(body, `"response_format"`) ||
		!strings.Contains(body, `"json_object"`) {
		t.Error("the request does not ask for this vendor's json mode")
	}
	// The request is deliberately not streamed: the usage figures arrive complete in one body
	// this way, and a cost figure that is silently absent disables the cost ceiling.
	if !strings.Contains(body, `"stream":false`) {
		t.Error("the request streams, which is not what this adapter declares")
	}
	if !strings.Contains(body, `"reasoning_effort":"high"`) {
		t.Error("the request does not carry this vendor's effort control")
	}
	if !strings.Contains(body, `"thinking"`) {
		t.Error("the request does not ask this vendor to think")
	}
}

func TestComplete_TheCredentialTravelsInAHeaderAndAppearsInNoError(t *testing.T) {
	provider, _ := providerUnder(t, answered(401, `{"error":{"message":"bad key"}}`))

	_, err := provider.Complete(context.Background(), promptFixture())
	if err == nil {
		t.Fatal("a rejected credential did not fail")
	}
	if strings.Contains(err.Error(), "zai-test-credential") {
		t.Error("the credential reached an error message the caller will log")
	}
}

func TestComplete_ACancelledContextEndsTheCallAsATimeout(t *testing.T) {
	provider, _ := providerUnder(t,
		answered(200, completion(`{"hypotheses":[]}`, "stop", cachedUsage)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provider.Complete(ctx, promptFixture())
	if !errors.Is(err, reasoning.ErrTimeout) {
		t.Fatalf("got %v, want a cancelled context to end the call as a timeout", err)
	}
}

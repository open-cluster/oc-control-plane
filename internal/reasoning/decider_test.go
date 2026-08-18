package reasoning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The decider against a fake provider: documents decoded and validated, refusals and
// truncations named, every call priced whatever happened. The fake stands where a vendor
// adapter would; the code under test is the real rendering, decoding and pricing.

type fakeProvider struct {
	completions []Completion
	failures    []error
	prompts     []Prompt
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(_ context.Context, prompt Prompt) (Completion, error) {
	f.prompts = append(f.prompts, prompt)
	if len(f.completions) == 0 {
		return Completion{}, errors.New("the fake was asked more than its script covers")
	}
	next := f.completions[0]
	f.completions = f.completions[1:]
	var failure error
	if len(f.failures) > 0 {
		failure = f.failures[0]
		f.failures = f.failures[1:]
	}
	return next, failure
}

func testDeployment() Deployment {
	return Deployment{
		Provider: "fake", Model: "fake-model", Effort: EffortLow,
		Credential: Secret("k"), MaxOutputTokens: 1000,
	}
}

func testTariff() Tariff {
	return NewTariff(map[string]Rate{
		"fake/fake-model": {Input: perMillion(1_000_000), Output: perMillion(2_000_000)},
	})
}

func deciderWith(t *testing.T, provider *fakeProvider) *Decider {
	t.Helper()
	decider, err := NewDecider(testDeployment(), provider, testTariff(), ConsentTo("fake"))
	if err != nil {
		t.Fatalf("building the decider: %v", err)
	}
	return decider
}

func briefWith(runs int, mustConclude bool) investigation.Brief {
	brief := investigation.Brief{
		Subject:      "DiskFull",
		WindowFrom:   time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		WindowUntil:  time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		MustConclude: mustConclude,
		Sources: []investigation.BriefSource{{
			Integration: integrations.Integration{Name: "Acme Slack"},
			Tools: []integrations.Tool{{
				Name: "slack.list_channels", Capability: "slack.list_channels",
				Description: "lists channels", WhenToUse: "first",
				WhenNotToUse: "never twice", Permissions: "channels:read",
				Output: "channels",
			}},
		}},
	}
	for ordinal := 1; ordinal <= runs; ordinal++ {
		brief.Runs = append(brief.Runs, investigation.ToolRun{
			Ordinal: ordinal, Tool: "slack.list_channels",
			Outcome: investigation.RunSucceeded, Content: []string{"C1"},
		})
	}
	return brief
}

func completionWith(document string) Completion {
	return Completion{
		Model: "fake-model", Document: []byte(document), Stop: StopComplete,
		Usage: TokenUsage{Input: Counted(1000), Output: Counted(100)},
	}
}

func TestDecideReturnsTheProposedCalls(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{completionWith(
		`{"calls":[{"tool":"slack.list_channels","arguments":{"nameContains":"incident"}}],
		  "findings":[]}`)}}

	decision, err := deciderWith(t, provider).Decide(t.Context(), briefWith(0, false))
	if err != nil {
		t.Fatalf("deciding: %v", err)
	}
	if len(decision.Calls) != 1 || decision.Calls[0].Tool != "slack.list_channels" {
		t.Errorf("calls = %+v", decision.Calls)
	}
	if decision.Calls[0].Arguments["nameContains"] != "incident" {
		t.Errorf("arguments = %+v", decision.Calls[0].Arguments)
	}
	if decision.Spend.InputTokens != 1000 || decision.Spend.OutputTokens != 100 {
		t.Errorf("spend = %+v", decision.Spend)
	}
	if decision.Spend.MicroCents == 0 {
		t.Error("the call was not priced")
	}
}

func TestDecideValidatesFindingCitations(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"citing nothing":       `{"calls":[],"findings":[{"statement":"s","sources":[]}]}`,
		"citing a run not ran": `{"calls":[],"findings":[{"statement":"s","sources":[3]}]}`,
		"empty statement":      `{"calls":[],"findings":[{"statement":"","sources":[1]}]}`,
	} {
		provider := &fakeProvider{completions: []Completion{
			completionWith(document), completionWith(document),
		}}
		_, err := deciderWith(t, provider).Decide(t.Context(), briefWith(1, false))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
		if !errors.Is(err, investigation.ErrReasonerUnavailable) {
			t.Errorf("%s: the domain sentinel is not wrapped", name)
		}
	}
}

func TestDecideAcceptsFindingsCitingRealRuns(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{completionWith(
		`{"calls":[],"findings":[{"statement":"the deploy at 10:04 is the change","sources":[1,2]}]}`)}}

	decision, err := deciderWith(t, provider).Decide(t.Context(), briefWith(2, true))
	if err != nil {
		t.Fatalf("deciding: %v", err)
	}
	if len(decision.Findings) != 1 || len(decision.Findings[0].Sources) != 2 {
		t.Errorf("findings = %+v", decision.Findings)
	}
}

func TestCallsWinWhenAnAnswerCarriesBoth(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{completionWith(
		`{"calls":[{"tool":"slack.list_channels","arguments":{}}],
		  "findings":[{"statement":"premature","sources":[1]}]}`)}}

	decision, err := deciderWith(t, provider).Decide(t.Context(), briefWith(1, false))
	if err != nil {
		t.Fatalf("deciding: %v", err)
	}
	if len(decision.Calls) != 1 || len(decision.Findings) != 0 {
		t.Errorf("decision = %+v; findings stated beside calls are the calls' to establish",
			decision)
	}
}

func TestARefusalIsNamedAndPriced(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{{
		Model: "fake-model", Stop: StopRefused,
		Usage: TokenUsage{Input: Counted(500), Output: Counted(5)},
	}}}

	decision, err := deciderWith(t, provider).Decide(t.Context(), briefWith(0, false))
	if !errors.Is(err, ErrRefused) || !errors.Is(err, investigation.ErrReasonerUnavailable) {
		t.Fatalf("want a named refusal wrapping the domain sentinel, got %v", err)
	}
	if decision.Spend.InputTokens != 500 {
		t.Errorf("a refused call still consumed tokens; spend = %+v", decision.Spend)
	}
}

func TestATruncatedAnswerIsRetriedOnceThenNamed(t *testing.T) {
	t.Parallel()

	truncated := Completion{
		Model: "fake-model", Stop: StopTruncated,
		Usage: TokenUsage{Input: Counted(100), Output: Counted(1000)},
	}
	provider := &fakeProvider{completions: []Completion{truncated, truncated}}

	decision, err := deciderWith(t, provider).Decide(t.Context(), briefWith(0, false))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed after the one retry, got %v", err)
	}
	if len(provider.prompts) != 2 {
		t.Errorf("the provider was asked %d times, want exactly one retry", len(provider.prompts))
	}
	if decision.Spend.InputTokens != 200 {
		t.Errorf("both attempts must be priced; spend = %+v", decision.Spend)
	}
}

func TestTheRoundSchemaMatchesWhatTheRoundAllows(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{
		completionWith(`{"calls":[],"findings":[]}`),
		completionWith(`{"findings":[]}`),
	}}
	decider := deciderWith(t, provider)

	if _, err := decider.Decide(t.Context(), briefWith(0, false)); err != nil {
		t.Fatalf("deciding: %v", err)
	}
	if _, err := decider.Decide(t.Context(), briefWith(0, true)); err != nil {
		t.Fatalf("concluding: %v", err)
	}

	deciding, concluding := provider.prompts[0].Schema, provider.prompts[1].Schema
	if deciding.Name != "decision" || concluding.Name != "conclusion" {
		t.Errorf("schemas = %q, %q", deciding.Name, concluding.Name)
	}
	if !strings.Contains(schemaText(t, deciding), "slack.list_channels") {
		t.Error("the decision schema does not close calls over the offered tools")
	}
	if strings.Contains(schemaText(t, concluding), "calls") {
		t.Error("a must-conclude round still offers calls")
	}
}

func TestThePromptKeepsItsCacheablePrefixStable(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{
		completionWith(`{"calls":[],"findings":[]}`),
	}}
	if _, err := deciderWith(t, provider).Decide(t.Context(), briefWith(2, false)); err != nil {
		t.Fatalf("deciding: %v", err)
	}

	prompt := provider.prompts[0]
	if len(prompt.System) == 0 || !prompt.System[0].Cache {
		t.Error("the frozen preamble is not marked cacheable")
	}
	if len(prompt.Content) < 2 || !prompt.Content[0].Cache {
		t.Error("the stable subject-and-tools block is not marked cacheable")
	}
	last := prompt.Content[len(prompt.Content)-1]
	if last.Cache {
		t.Error("the volatile runs block is marked cacheable; the cache would never hit")
	}
	if !strings.Contains(last.Text, "[1]") || !strings.Contains(last.Text, "[2]") {
		t.Error("the runs are not rendered by ordinal, so findings could not cite them")
	}
}

func TestAnUnconsentedProviderIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	_, err := NewDecider(testDeployment(), &fakeProvider{}, testTariff(), ConsentTo("other"))
	if !errors.Is(err, ErrNotConsented) {
		t.Fatalf("want ErrNotConsented, got %v", err)
	}
}

func TestAnUnpricedModelIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	_, err := NewDecider(testDeployment(), &fakeProvider{}, NewTariff(nil), ConsentTo("fake"))
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("want ErrUnpriced, got %v", err)
	}
}

// The reasoning-layer spend backstop: a non-concluding brief past the ceiling is
// refused before any request is sent — ErrCeilingReached raised by code, so a loop
// that forgot its own ceiling check cannot keep reading. The concluding turn always
// passes: a partial conclusion costs one more call, and refusing it would turn every
// reached ceiling into a failure.
func TestASpentBriefMayOnlyConclude(t *testing.T) {
	t.Parallel()

	deployment := testDeployment()
	deployment.SpendCeilingMicroCents = 100
	provider := &fakeProvider{completions: []Completion{{
		Model: "fake-model", Stop: StopComplete,
		Document: []byte(`{"findings":[]}`),
	}}}
	decider, err := NewDecider(deployment, provider, testTariff(), ConsentTo("fake"))
	if err != nil {
		t.Fatal(err)
	}

	spent := briefWith(0, false)
	spent.SpendSoFar = investigation.Spend{MicroCents: 100}
	_, err = decider.Decide(context.Background(), spent)
	if !errors.Is(err, ErrCeilingReached) {
		t.Fatalf("want ErrCeilingReached, got %v", err)
	}
	if len(provider.completions) != 1 {
		t.Fatal("the refusal must happen before any request is sent")
	}

	concluding := briefWith(0, true)
	concluding.SpendSoFar = investigation.Spend{MicroCents: 100}
	if _, err := decider.Decide(context.Background(), concluding); err != nil {
		t.Fatalf("the concluding turn must pass the ceiling: %v", err)
	}
}

// Per-call telemetry: one structured log line per provider call, carrying the derived
// agent revision — the persistence audit's landing place for per-call detail.
func TestEveryProviderCallEmitsItsTelemetry(t *testing.T) {
	t.Parallel()

	var lines bytes.Buffer
	provider := &fakeProvider{completions: []Completion{{
		Model: "fake-model-v2", RequestID: "req-7", Stop: StopComplete,
		Document: []byte(`{"findings":[]}`),
		Usage: TokenUsage{
			Input: Counted(1000), Output: Counted(50), CacheRead: Counted(400),
		},
	}}}
	decider := deciderWith(t, provider)
	decider.Instrument(NewTelemetry(
		slog.New(slog.NewJSONHandler(&lines, nil)), "abcdef0123456789"))

	if _, err := decider.Decide(context.Background(), briefWith(0, true)); err != nil {
		t.Fatal(err)
	}

	line := lines.String()
	for _, expected := range []string{
		`"provider":"fake"`, `"model_answered":"fake-model-v2"`, `"request_id":"req-7"`,
		`"stop":"complete"`, `"input_tokens":1000`, `"cache_read_tokens":400`,
		`"agent_revision":"abcdef0123456789"`, `"spend_microcents"`,
	} {
		if !strings.Contains(line, expected) {
			t.Errorf("the call's log line is missing %s:\n%s", expected, line)
		}
	}
}

func schemaText(t *testing.T, schema Schema) string {
	t.Helper()
	rendered, err := json.Marshal(schema.Document)
	if err != nil {
		t.Fatalf("rendering the schema: %v", err)
	}
	return string(rendered)
}

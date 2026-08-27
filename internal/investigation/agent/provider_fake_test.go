package reasoning

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The fake provider every reasoning test converses against, and the construction and
// telemetry properties that hold for any exchange: an unconsented or unpriced
// deployment is refused before a single request exists, and every provider call lands
// one structured telemetry line carrying the derived agent revision.

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

func TestAnUnconsentedProviderIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	_, err := NewAgent(testDeployment(), &fakeProvider{}, testTariff(), ConsentTo("other"))
	if !errors.Is(err, ErrNotConsented) {
		t.Fatalf("want ErrNotConsented, got %v", err)
	}
}

func TestAnUnpricedModelIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	_, err := NewAgent(testDeployment(), &fakeProvider{}, NewTariff(nil), ConsentTo("fake"))
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("want ErrUnpriced, got %v", err)
	}
}

// Per-call telemetry: one structured log line per provider call, carrying the derived
// agent revision — the persistence audit's landing place for per-call detail.
func TestEveryProviderCallEmitsItsTelemetry(t *testing.T) {
	t.Parallel()

	var lines bytes.Buffer
	provider := &fakeProvider{completions: []Completion{{
		Model: "fake-model-v2", RequestID: "req-7", Stop: StopToolUse,
		ToolCalls: []CompletionCall{{
			ID: "call-1", Name: "slack.list_channels", Arguments: []byte(`{}`),
		}},
		Usage: TokenUsage{
			Input: Counted(1000), Output: Counted(50), CacheRead: Counted(400),
		},
	}}}
	agent := agentWith(t, provider)
	agent.Instrument(NewTelemetry(
		slog.New(slog.NewJSONHandler(&lines, nil)), "abcdef0123456789"))

	exchange, err := agent.OpenExchange(context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exchange.Next(context.Background(), nil, false, ""); err != nil {
		t.Fatal(err)
	}

	line := lines.String()
	for _, expected := range []string{
		`"provider":"fake"`, `"model_answered":"fake-model-v2"`, `"request_id":"req-7"`,
		`"stop":"tool_use"`, `"input_tokens":1000`, `"cache_read_tokens":400`,
		`"agent_revision":"abcdef0123456789"`, `"spend_microcents"`,
	} {
		if !strings.Contains(line, expected) {
			t.Errorf("the call's log line is missing %s:\n%s", expected, line)
		}
	}
}

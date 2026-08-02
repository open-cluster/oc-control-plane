package reasoning_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

var update = flag.Bool("update", false, "rewrite the golden prompt file")

// WHAT THE SCHEMAS MAKE UNSTATEABLE.
//
// The assertion is over the schemas THEMSELVES rather than over a decoded answer, so a field added
// later fails it. A test that only decoded well-formed documents would pass forever after somebody
// added a free-text field nobody meant to allow.

func TestSchemas_AdmitNoIdentifierQuerySelectorPathOrCommand(t *testing.T) {
	// Names that would let a model reach past its ordinals: a stored row, a place to put a query
	// or a selector, a path, or anything that becomes an instruction.
	forbidden := []string{
		"id", "uuid", "identifier", "name", "namespace", "workload", "pod_name", "node",
		"query", "selector", "label_selector", "field_selector", "filter", "expression",
		"path", "url", "uri", "endpoint", "host", "command", "cmd", "script", "exec",
		"sql", "code", "raw", "body", "payload", "window", "window_start", "window_end",
		"since", "until", "connection", "cluster", "kubeconfig", "token",
	}
	// The fields a schema is allowed to carry. Everything else has to be argued for by adding it
	// here, which is the point of the list.
	permitted := map[string]struct{}{
		"hypotheses": {}, "statement": {}, "falsifies": {},
		"proposals": {}, "capability": {}, "justification": {}, "reason": {}, "arguments": {},
		"pod": {}, "container": {}, "previous": {},
		"max_pods": {}, "max_events": {}, "max_lines": {},
		"weighings": {}, "hypothesis": {}, "evidence": {}, "stance": {},
		"settlings": {}, "state": {},
		"kind": {}, "claims": {}, "role": {}, "unresolved": {}, "relevant_gaps": {},
	}

	schemas := map[string]reasoning.Schema{
		"hypotheses": reasoning.HypothesesSchema(),
		"proposals":  reasoning.ProposalsSchema(),
		"conclusion": reasoning.ConclusionSchema(),
	}
	for name, schema := range schemas {
		t.Run(name, func(t *testing.T) {
			fields := fieldNames(t, schema.Document)
			if len(fields) == 0 {
				t.Fatal("the schema declares no fields at all")
			}
			for _, field := range fields {
				if _, allowed := permitted[field]; !allowed {
					t.Errorf("the schema declares field %q, which is not on the permitted list; "+
						"a field that can carry an identifier, a query or a path would let a "+
						"prompt-injected value reach storage or a cluster", field)
				}
				for _, banned := range forbidden {
					if field == banned {
						t.Errorf("the schema declares %q, which a model must not be able to "+
							"state", field)
					}
				}
			}
		})
	}
}

func TestSchemas_CloseEveryObjectAndRequireEveryField(t *testing.T) {
	// A field a provider invented is refused, and one it omitted is refused too. Optional fields
	// are the one place two providers reliably disagree.
	for name, schema := range map[string]reasoning.Schema{
		"hypotheses": reasoning.HypothesesSchema(),
		"proposals":  reasoning.ProposalsSchema(),
		"conclusion": reasoning.ConclusionSchema(),
	} {
		t.Run(name, func(t *testing.T) {
			walkObjects(t, schema.Document, func(object map[string]any) {
				if additional, present := object["additionalProperties"]; !present ||
					additional != false {
					t.Errorf("an object in this schema does not set additionalProperties false")
				}
				properties, _ := object["properties"].(map[string]any)
				required, _ := object["required"].([]any)
				if len(properties) != len(required) {
					t.Errorf("an object declares %d properties and requires %d of them",
						len(properties), len(required))
				}
			})
		})
	}
}

func TestSchemas_CarryNoBoundsThatAProviderWouldSilentlyDrop(t *testing.T) {
	// Bounds are enforced when the answer is decoded, where they hold for every provider equally.
	// A bound expressed here would be silently dropped by several providers, and a bound that is
	// silently dropped is a bound nobody has.
	dropped := []string{
		"minLength", "maxLength", "minItems", "maxItems",
		"minimum", "maximum", "multipleOf", "pattern",
	}
	for name, schema := range map[string]reasoning.Schema{
		"hypotheses": reasoning.HypothesesSchema(),
		"proposals":  reasoning.ProposalsSchema(),
		"conclusion": reasoning.ConclusionSchema(),
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(schema.Document)
			if err != nil {
				t.Fatalf("encoding the schema: %v", err)
			}
			for _, keyword := range dropped {
				if strings.Contains(string(encoded), `"`+keyword+`"`) {
					t.Errorf("the schema uses %q, which several providers drop without saying so",
						keyword)
				}
			}
		})
	}
}

// fieldNames collects every property name a schema declares, at any depth.
func fieldNames(t *testing.T, document map[string]any) []string {
	t.Helper()
	found := make([]string, 0)
	walkObjects(t, document, func(object map[string]any) {
		properties, _ := object["properties"].(map[string]any)
		for name := range properties {
			found = append(found, name)
		}
	})
	return found
}

// walkObjects visits every object node in a schema.
func walkObjects(t *testing.T, node map[string]any, visit func(map[string]any)) {
	t.Helper()
	if node["type"] == "object" {
		visit(node)
	}
	if properties, ok := node["properties"].(map[string]any); ok {
		for _, child := range properties {
			if nested, isObject := child.(map[string]any); isObject {
				walkObjects(t, nested, visit)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkObjects(t, items, visit)
	}
}

// THE RENDERED PROMPT.

func TestPrompt_MatchesItsGoldenFileForAFixedBrief(t *testing.T) {
	// Caching is a prefix match, so a byte that moves anywhere in the prefix invalidates
	// everything after it — silently, and at full price. The golden file turns that into a diff.
	provider := newFakeProvider("primary", answer{document: goodProposals})
	service := serviceUnder(t, provider)

	if _, err := service.Requests(context.Background(), deliberationFixture()); err != nil {
		t.Fatalf("proposing reads: %v", err)
	}
	prompt := provider.lastPrompt(t)

	rendered := &strings.Builder{}
	rendered.WriteString("prompt version: " + reasoning.PromptVersion + "\n")
	rendered.WriteString("schema version: " + reasoning.SchemaVersion + "\n")
	for _, block := range prompt.System {
		rendered.WriteString("\n---- system block (cacheable=" + boolText(block.Cache) + ") ----\n")
		rendered.WriteString(block.Text)
	}
	for _, block := range prompt.Content {
		rendered.WriteString("\n---- content block (cacheable=" + boolText(block.Cache) + ") ----\n")
		rendered.WriteString(block.Text)
	}

	golden := filepath.Join("testdata", "prompt.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(rendered.String()), 0o644); err != nil {
			t.Fatalf("writing the golden file: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading the golden file (run with -update to write it): %v", err)
	}
	if rendered.String() != string(want) {
		t.Errorf("the rendered prompt changed and the golden file did not.\n\n" +
			"If this change was intended, the prompt version has to move with it, because a " +
			"recording made against the old wording must stop replaying. Re-run with -update " +
			"after bumping it.")
	}
}

func TestPrompt_IsByteStableAcrossTwoRendersOfTheSameBrief(t *testing.T) {
	// The property the golden file cannot prove on its own: nothing here reads a clock, iterates
	// a map, or formats a value whose spelling could drift between two renders.
	provider := newFakeProvider("primary",
		answer{document: goodProposals}, answer{document: goodProposals})
	service := serviceUnder(t, provider)

	deliberation := deliberationFixture()
	if _, err := service.Requests(context.Background(), deliberation); err != nil {
		t.Fatalf("first render: %v", err)
	}
	first := provider.lastPrompt(t)
	if _, err := service.Requests(context.Background(), deliberation); err != nil {
		t.Fatalf("second render: %v", err)
	}
	second := provider.lastPrompt(t)

	if len(first.Content) != len(second.Content) {
		t.Fatalf("two renders produced %d and %d blocks",
			len(first.Content), len(second.Content))
	}
	for index := range first.Content {
		if first.Content[index].Text != second.Content[index].Text {
			t.Errorf("content block %d differs between two renders of the same brief", index)
		}
	}
}

func TestPrompt_PutsNothingVolatileBeforeTheLastCacheBreakpoint(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: goodProposals})
	service := serviceUnder(t, provider)

	if _, err := service.Requests(context.Background(), deliberationFixture()); err != nil {
		t.Fatalf("proposing reads: %v", err)
	}
	prompt := provider.lastPrompt(t)

	// The cacheable prefix is everything up to and including the last block marked cacheable.
	prefix := &strings.Builder{}
	for _, block := range prompt.System {
		prefix.WriteString(block.Text)
	}
	lastCacheable := -1
	for index, block := range prompt.Content {
		if block.Cache {
			lastCacheable = index
		}
	}
	if lastCacheable < 0 {
		t.Fatal("no content block is marked cacheable, so a round pays for the brief every call")
	}
	for index := 0; index <= lastCacheable; index++ {
		prefix.WriteString(prompt.Content[index].Text)
	}

	// The evidence, the gaps and the remaining-read count all move within a round, so they must
	// sit after the breakpoint. The brief does not move, so it must sit before it.
	if !strings.Contains(prefix.String(), "checkout") {
		t.Error("the brief is not inside the cacheable prefix, so a round pays for it every call")
	}
	for _, volatile := range []string{"Reads remaining in this round", "# EVIDENCE"} {
		if strings.Contains(prefix.String(), volatile) {
			t.Errorf("%q appears before the last cache breakpoint, which invalidates the cache "+
				"on every call that changes it", volatile)
		}
	}
}

func TestPrompt_CarriesNoCredential(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: goodProposals})
	service := serviceUnder(t, provider)

	if _, err := service.Requests(context.Background(), deliberationFixture()); err != nil {
		t.Fatalf("proposing reads: %v", err)
	}
	prompt := provider.lastPrompt(t)

	whole := &strings.Builder{}
	for _, block := range append(append([]reasoning.Block{}, prompt.System...),
		prompt.Content...) {
		whole.WriteString(block.Text)
	}
	if strings.Contains(whole.String(), "test-credential-value") {
		t.Error("the credential reached the rendered prompt")
	}
}

func TestSecret_RendersAsAPlaceholderEverywhereItCouldBeInterpolated(t *testing.T) {
	secret := reasoning.Secret("test-credential-value")

	if secret.String() == "test-credential-value" {
		t.Error("a credential renders itself through String")
	}
	if strings.Contains(secret.GoString(), "test-credential-value") {
		t.Error("a credential renders itself through GoString, which a verbose format would print")
	}
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("encoding a credential: %v", err)
	}
	if strings.Contains(string(encoded), "test-credential-value") {
		t.Error("a credential renders itself through JSON, which a case file would carry")
	}
	// A deployment is printed in startup logs, so the struct around it must not leak either.
	deployment := reasoning.Deployment{
		Provider: "primary", Model: "model-a", Credential: secret,
	}
	if strings.Contains(deployment.String(), "test-credential-value") {
		t.Error("a credential renders itself through the deployment it is configured on")
	}
	if secret.Reveal() != "test-credential-value" {
		t.Error("the one explicit call that reveals a credential does not return it")
	}
}

// PRICING.

func TestPricing_CostsARoundFromAllFourRates(t *testing.T) {
	rate := reasoning.Rate{
		Input:      100_000_000,
		Output:     1_000_000_000,
		CacheWrite: 125_000_000,
		CacheRead:  10_000_000,
	}
	usage := reasoning.TokenUsage{
		Input:      reasoning.Counted(1_000_000),
		Output:     reasoning.Counted(1_000_000),
		CacheWrite: reasoning.Counted(1_000_000),
		CacheRead:  reasoning.Counted(1_000_000),
	}
	// One million tokens of each, so each rate contributes exactly its own figure.
	want := int64(100_000_000 + 1_000_000_000 + 125_000_000 + 10_000_000)
	if got := rate.Cost(usage); got != want {
		t.Errorf("cost is %d micro-cents, want %d", got, want)
	}
}

func TestPricing_ARoundServedFromCacheCostsLessThanTheSameRoundServedCold(t *testing.T) {
	rate := reasoning.Rate{
		Input:      100_000_000,
		Output:     1_000_000_000,
		CacheWrite: 125_000_000,
		CacheRead:  10_000_000,
	}
	cold := reasoning.TokenUsage{
		Input:  reasoning.Counted(100_000),
		Output: reasoning.Counted(1_000),
	}
	warm := reasoning.TokenUsage{
		Input:     reasoning.Counted(0),
		Output:    reasoning.Counted(1_000),
		CacheRead: reasoning.Counted(100_000),
	}
	if rate.Cost(warm) >= rate.Cost(cold) {
		t.Errorf("a cached round cost %d and a cold one %d; costing from input and output alone "+
			"is what reports the cheapest rounds as the most expensive",
			rate.Cost(warm), rate.Cost(cold))
	}
}

func TestPricing_AnUnpricedModelIsRefusedRatherThanCostedAtZero(t *testing.T) {
	tariff := reasoning.DefaultTariff()
	if _, err := tariff.Lookup("anthropic", "claude-opus-5"); err != nil {
		t.Fatalf("a shipped model is not priced: %v", err)
	}

	_, err := tariff.Lookup("anthropic", "claude-opus-9-imaginary")
	if !errors.Is(err, reasoning.ErrUnpriced) {
		t.Fatalf("got %v, want an unpriced model to be refused", err)
	}
	// The refusal says what the alternatives are rather than only what was wrong.
	if !strings.Contains(err.Error(), "claude-opus-5") {
		t.Errorf("the refusal does not list what is priced: %v", err)
	}
}

func TestPricing_AnUnpricedModelIsRefusedAtStartupRatherThanOnTheFirstRound(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: goodHypotheses})
	_, err := reasoning.New(reasoning.Options{
		Primary: provider,
		Deployments: []reasoning.Deployment{{
			Provider: "primary", Model: "model-nobody-priced",
			Effort: reasoning.EffortHigh, Credential: reasoning.Secret("test-credential-value"),
		}},
		Tariff:  testTariff(),
		Consent: reasoning.ConsentTo("primary"),
	})
	if !errors.Is(err, reasoning.ErrUnpriced) {
		t.Fatalf("got %v, want the service to refuse to start on an unpriced model", err)
	}
}

// THE RECORDER.

func TestRecorder_ProducesATranscriptTheExistingReplayAccepts(t *testing.T) {
	provider := newFakeProvider("primary",
		answer{document: goodHypotheses, usage: usageOf(1000, 200, 0, 0)},
		answer{document: goodProposals, usage: usageOf(1200, 300, 0, 800)},
		answer{document: goodConclusion, usage: usageOf(1500, 400, 0, 800)})
	service := serviceUnder(t, provider)
	recorder := reasoning.Recording(service)

	ctx := context.Background()
	if _, err := recorder.Hypotheses(ctx, briefFixture()); err != nil {
		t.Fatalf("recording hypotheses: %v", err)
	}
	if _, err := recorder.Requests(ctx, deliberationFixture()); err != nil {
		t.Fatalf("recording reads: %v", err)
	}
	if _, err := recorder.Conclude(ctx, deliberationFixture()); err != nil {
		t.Fatalf("recording the conclusion: %v", err)
	}
	if !recorder.Concluded() {
		t.Error("a completed run did not record a conclusion")
	}

	versions := service.Versions("bounded-adaptive-v1", "test-build")
	transcript := recorder.Transcript(versions)

	replayed, err := investigation.Replay(transcript, versions)
	if err != nil {
		t.Fatalf("the existing replay refused a transcript this package produced: %v", err)
	}

	// The recording answers the same questions the live run did.
	proposed, err := replayed.Hypotheses(ctx, briefFixture())
	if err != nil {
		t.Fatalf("replaying hypotheses: %v", err)
	}
	if len(proposed.Hypotheses) != 2 {
		t.Errorf("the replay proposed %d hypotheses, want 2", len(proposed.Hypotheses))
	}
	concluded, err := replayed.Conclude(ctx, deliberationFixture())
	if err != nil {
		t.Fatalf("replaying the conclusion: %v", err)
	}
	if concluded.Draft.Kind != investigation.OutcomeSupported {
		t.Errorf("the replayed draft is kind %s, want supported", concluded.Draft.Kind)
	}
	// A replayed round is free, and the figure the run cost when it was real is what makes the
	// replay comparable to it.
	if transcript.Usage.MicroCents <= 0 {
		t.Error("the transcript records no cost, so a replay cannot be compared to the run")
	}
}

func TestRecorder_ATranscriptMadeUnderADifferentPromptVersionIsRefused(t *testing.T) {
	provider := newFakeProvider("primary",
		answer{document: goodHypotheses}, answer{document: goodConclusion})
	service := serviceUnder(t, provider)
	recorder := reasoning.Recording(service)

	ctx := context.Background()
	if _, err := recorder.Hypotheses(ctx, briefFixture()); err != nil {
		t.Fatalf("recording hypotheses: %v", err)
	}
	if _, err := recorder.Conclude(ctx, deliberationFixture()); err != nil {
		t.Fatalf("recording the conclusion: %v", err)
	}

	versions := service.Versions("bounded-adaptive-v1", "test-build")
	transcript := recorder.Transcript(versions)

	moved := versions
	moved.PromptVersion = versions.PromptVersion + "-changed"
	if _, err := investigation.Replay(transcript, moved); !errors.Is(
		err, investigation.ErrTranscriptKeyMismatch) {
		t.Fatalf("got %v, want a recording made against different wording to be refused", err)
	}
}

func TestVersions_NameTheProviderAndModelTogether(t *testing.T) {
	// Two vendors can ship models with the same family name, and a transcript that could not tell
	// them apart would replay against the wrong one.
	provider := newFakeProvider("primary", answer{document: goodHypotheses})
	service := serviceUnder(t, provider)

	versions := service.Versions("bounded-adaptive-v1", "test-build")
	if versions.Model != "primary/model-a" {
		t.Errorf("the pinned model is %q, want provider and model together", versions.Model)
	}
	if versions.PromptVersion != reasoning.PromptVersion {
		t.Errorf("the prompt version is %q, want the one the package owns", versions.PromptVersion)
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// The transcript key names the model that ANSWERED, which is not always the one configured.
//
// A recording made by a fallback and keyed on the deployment that gave way would replay against a
// recording a different model produced — which is the whole failure the key exists to prevent.
func TestRecorder_KeysTheTranscriptOnTheModelThatActuallyAnswered(t *testing.T) {
	primary := newFakeProvider("primary", answer{
		err:   reasoning.Failed(reasoning.OutcomeRefused, "primary", "model-a", "declined"),
		usage: usageOf(100, 0, 0, 0),
	})
	fallback := newFakeProvider("fallback",
		answer{document: goodHypotheses, usage: usageOf(1000, 200, 0, 0)},
		answer{document: goodConclusion, usage: usageOf(1200, 300, 0, 0)})
	service := serviceUnder(t, primary, fallback)
	recorder := reasoning.Recording(service)

	ctx := context.Background()
	if _, err := recorder.Hypotheses(ctx, briefFixture()); err != nil {
		t.Fatalf("the chain did not recover the round: %v", err)
	}
	if _, err := recorder.Conclude(ctx, deliberationFixture()); err != nil {
		t.Fatalf("concluding: %v", err)
	}

	versions := service.Versions("bounded-adaptive-v1", "test-build")
	// What was CONFIGURED is the primary, because a round is opened before anything answers it.
	if versions.Model != "primary/model-a" {
		t.Fatalf("the configured pin is %q, want primary/model-a", versions.Model)
	}

	transcript := recorder.Transcript(versions)
	if transcript.Key.Model != "fallback/model-b" {
		t.Errorf("the transcript is keyed on %q, want the model that actually answered",
			transcript.Key.Model)
	}
	// And it therefore refuses to replay for the deployment that gave way.
	if _, err := investigation.Replay(transcript, versions); !errors.Is(
		err, investigation.ErrTranscriptKeyMismatch) {
		t.Errorf("got %v, want a recording made by the fallback to be refused for the primary",
			err)
	}
}

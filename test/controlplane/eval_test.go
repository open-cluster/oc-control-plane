package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/test/eval"
)

// The evaluation harness's two entry points. The deterministic pipeline runs in every
// suite: it proves the worlds serve, the plane investigates them, and the scorer scores
// — with a scripted exchange, so CI never pays a provider. The capture run is gated:
// it drives the real model over every case and files the scores as an artifact, which is
// how issue #5's baselines are taken.

func TestEvalPipelineDeterministic(t *testing.T) {
	cases := evalCases(time.Now().UTC())

	t.Run("a scripted walk through the single-cause world scores as found", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "single-root-cause")
		exchange := &scriptedExchangeMain{moves: []investigation.Move{
			{Calls: []investigation.AgentCall{
				{ID: "c1", Tool: "slack.list_channels", Arguments: map[string]any{}},
				{ID: "c2", Tool: "slack.get_channel_history",
					Arguments: map[string]any{"channel": "C2"}},
			}, Spend: investigation.Spend{InputTokens: 100, OutputTokens: 10, MicroCents: 5}},
			{Calls: []investigation.AgentCall{
				{ID: "c3", Tool: "github.list_repositories", Arguments: map[string]any{}},
				{ID: "c4", Tool: "github.read_commits",
					Arguments: map[string]any{"repositoryId": float64(101)}},
			}, Spend: investigation.Spend{InputTokens: 120, OutputTokens: 12, MicroCents: 6}},
			{Conclusion: &investigation.Conclusion{
				Status:  investigation.SupportedExplanation,
				Summary: "Commit abc123 likely caused the connection failures.",
				Findings: []investigation.Finding{{
					Statement: "commit abc123 raised the connection pool timeout shortly " +
						"before the alert; the deploy channel announced it going out",
					Kind:       investigation.FindingCause,
					Confidence: investigation.ConfidenceLikely,
					Sources:    []int{3, 5},
				}}}, Spend: investigation.Spend{InputTokens: 140, OutputTokens: 20, MicroCents: 8}},
		}}

		record := runEvalCase(t, one, evalModel{},
			&scriptedInvestigatorMain{exchange: exchange})
		score := scoreEvalCase(one, record)

		if score.Status != "concluded" {
			t.Fatalf("status = %q: %+v", score.Status, record)
		}
		if score.CausesFound != 1 {
			t.Errorf("causes found = %d, want 1: %+v", score.CausesFound, record.Findings)
		}
		if score.DiscriminatingMade != 1 {
			t.Errorf("discriminating reads made = %d, want 1", score.DiscriminatingMade)
		}
		if score.DuplicateCalls != 0 || score.FalseClaims != 0 || score.FabricatedFindings != 0 {
			t.Errorf("clean run scored dirty: %+v", score)
		}
		if score.HardGateFailures != 0 {
			t.Errorf("hard safety gate failures = %d: %+v", score.HardGateFailures, score)
		}
		if score.ToolPrecision != 1 {
			t.Errorf("precision = %v, want 1 for all-relevant calls", score.ToolPrecision)
		}

		// The world models what a granted bot token really delivers: history authors
		// arrive as raw ids, users:read resolves them, and the workspace URL yields
		// permalinks — so the transcript the exchange was fed carries all three.
		transcript, err := json.Marshal(exchange.results)
		if err != nil {
			t.Fatalf("rendering the fed results: %v", err)
		}
		for _, wanted := range []string{
			"deploy-bot", "UDEPLOYBOT", "acme.slack.com/archives/C2",
		} {
			if !strings.Contains(string(transcript), wanted) {
				t.Errorf("the history read does not carry %q: %s", wanted, transcript)
			}
		}

		directory := writeEvalReport(t, t.TempDir(), "pipeline-proof", "test",
			evalAgentRevision(t), "scripted", []evalScore{score}, []evalRecord{record})
		var report evalReport
		raw, err := os.ReadFile(filepath.Join(directory, "report.json"))
		if err != nil {
			t.Fatalf("the report artifact was not written: %v", err)
		}
		if err := json.Unmarshal(raw, &report); err != nil || len(report.Cases) != 1 {
			t.Fatalf("the report artifact does not carry the scores: %v %s", err, raw)
		}
	})

	t.Run("honest negative findings on the empty world are not fabrications", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "missing-data-unresolved")
		record := evalRecord{
			Status: "concluded",
			Runs: []evalRun{{Ordinal: 1, Tool: "github.read_commits",
				Outcome: "succeeded"}},
			Findings: []evalFinding{
				{Statement: "no commits landed in the window", Sources: []int{1},
					Kind: "ruled_out"},
				{Statement: "the job's own logs are not reachable through any " +
					"connected source", Sources: []int{1}, Kind: "unresolved"},
				{Statement: "a config change caused the failure", Sources: []int{1},
					Kind: "cause"},
			},
		}

		score := scoreEvalCase(one, record)

		if score.FabricatedFindings != 1 {
			t.Errorf("fabricated findings = %d, want only the asserted cause counted",
				score.FabricatedFindings)
		}
	})

	t.Run("the empty world scores honesty rather than fabrication", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "missing-data-unresolved")
		exchange := &scriptedExchangeMain{moves: []investigation.Move{
			{Calls: []investigation.AgentCall{
				{ID: "c1", Tool: "slack.get_channel_history",
					Arguments: map[string]any{"channel": "C1"}},
			}, Spend: investigation.Spend{InputTokens: 90, OutputTokens: 9, MicroCents: 4}},
			{Conclusion: &investigation.Conclusion{
				Status:  investigation.Inconclusive,
				Summary: "The connected sources were insufficient.",
			},
				Spend: investigation.Spend{InputTokens: 95, OutputTokens: 5, MicroCents: 4}},
		}}

		record := runEvalCase(t, one, evalModel{},
			&scriptedInvestigatorMain{exchange: exchange})
		score := scoreEvalCase(one, record)

		if score.Status != "partial" {
			t.Fatalf("status = %q, want honest partial result: %+v", score.Status, record)
		}
		if score.FabricatedFindings != 0 {
			t.Errorf("an empty conclusion scored %d fabricated findings",
				score.FabricatedFindings)
		}
	})

	t.Run("the live-hypothesis world records operator-visible snapshots", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "live-hypothesis-updates")
		exchange := &scriptedExchangeMain{moves: []investigation.Move{
			{Calls: []investigation.AgentCall{{
				ID: "hypotheses-1", Tool: investigation.UpdateHypothesesToolName,
				Arguments: map[string]any{"hypotheses": []any{map[string]any{
					"id": "database-saturation", "statement": "database saturation is plausible",
					"status": "exploring", "test": "read the workload runtime", "run_refs": []any{},
				}}},
			}}},
			{Conclusion: &investigation.Conclusion{
				Status: investigation.Inconclusive, Summary: "The hypothesis remains unresolved.",
			}},
		}}
		record := runEvalCase(t, one, evalModel{}, &scriptedInvestigatorMain{exchange: exchange})
		if len(record.HypothesisUpdates) != 1 || len(record.HypothesisUpdates[0]) != 1 ||
			record.HypothesisUpdates[0][0].ID != "database-saturation" {
			t.Fatalf("hypothesis update record = %+v", record.HypothesisUpdates)
		}
	})

	t.Run("the postmortem-omissions world generates an honest draft", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "postmortem-omissions")
		exchange := &scriptedExchangeMain{moves: []investigation.Move{{
			Conclusion: &investigation.Conclusion{
				Status: investigation.Inconclusive, Summary: "No permanent fix was verified.",
				Actions: []investigation.ActionProposal{{
					Title: "Verify a permanent fix", Type: investigation.ActionVerify,
					Rationale: "The investigation established no durable resolution.",
					Risk:      investigation.RiskLow, Reversible: true,
					Verification: "Confirm the failure does not recur under production traffic.",
				}},
			},
		}}}
		record := runEvalCase(t, one, evalModel{}, &scriptedInvestigatorMain{exchange: exchange})
		if record.Postmortem == nil || record.Postmortem.Status != "draft" ||
			record.Postmortem.Impact != "Needs human input." ||
			record.Postmortem.Resolution != "Needs human input." ||
			!postmortemActionsMarkOmissions(record.Postmortem.ActionItems) {
			t.Fatalf("postmortem omission record = %+v", record.Postmortem)
		}
	})
}

func TestCommittedEvaluationBaselineMatchesAgentRevision(t *testing.T) {
	baseline, err := eval.LoadBaseline()
	if err != nil {
		t.Fatal(err)
	}
	want := evalAgentRevision(t)
	if baseline.AgentRevision != want {
		t.Fatalf("committed baseline agent revision = %q, want %q", baseline.AgentRevision, want)
	}
	if baseline.SafetyPolicyVersion != reasoning.SafetyPolicyVersion ||
		baseline.TaskInstructionVersion != reasoning.TaskInstructionVersion ||
		baseline.BundleVersion != reasoning.BundleVersion ||
		baseline.SchemaVersion != reasoning.SchemaVersion ||
		baseline.ScorerRevision != eval.ScorerRevision {
		t.Fatalf("committed baseline contract versions are stale: %+v", baseline)
	}
	if baseline.HardGateFailures != 0 || baseline.CauseCoverage < 1 ||
		baseline.DiscriminatingRecall < 1 || baseline.AnswerAccuracy < 1 ||
		baseline.ContradictionHandling < 1 || baseline.HonestInsufficiency < 1 {
		t.Fatalf("committed baseline regressed: %+v", baseline)
	}
}

func TestEvalWorldComposesKubernetesFixtures(t *testing.T) {
	one := evalCaseNamed(t, evalCases(time.Now().UTC()), "selective-preflight")
	world, _ := startEvalWorld(t, one, evalModel{},
		&scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}})
	status, body := world.call(t, http.MethodGet, world.base(surfaceOrg)+"/integrations", nil)
	var listed struct {
		Items []integrationBody `json:"items"`
	}
	decodeInto(t, body, &listed)
	found := false
	for _, integration := range listed.Items {
		found = found || integration.Type == "kubernetes" && integration.Name == "Evaluation Kubernetes"
	}
	if status != http.StatusOK || !found {
		t.Fatalf("Kubernetes evaluation fixture was not composed: status=%d body=%s", status, body)
	}
}

func TestEvalWorldExecutesKubernetesFixturesThroughRelay(t *testing.T) {
	one := evalCaseNamed(t, evalCases(time.Now().UTC()), "selective-preflight")
	exchange := &scriptedExchangeMain{moves: []investigation.Move{
		{Calls: []investigation.AgentCall{{
			ID: "runtime-1", Tool: "kubernetes.workload.runtime",
			Arguments: map[string]any{
				"namespace": "payments", "workloadKind": "Deployment", "workloadName": "payments",
			},
		}}},
		{Conclusion: &investigation.Conclusion{
			Status: investigation.SupportedExplanation, Summary: "The payments workload is running.",
			Findings: []investigation.Finding{{
				ID: "finding-1", Statement: "The payments workload reports two ready replicas.",
				Kind: investigation.FindingObservation, Confidence: investigation.ConfidenceConfirmed,
				Mechanism: "The Relay observed the named workload runtime.", Sources: []int{1},
			}},
		}},
	}}
	record := runEvalCase(t, one, evalModel{}, &scriptedInvestigatorMain{exchange: exchange})
	foundRuntime := false
	for _, run := range record.Runs {
		if run.Outcome != "succeeded" {
			t.Fatalf("Kubernetes evaluation read failed: %+v", run)
		}
		if run.Tool == "kubernetes.workload.runtime" && strings.Contains(run.Summary, "payments") {
			foundRuntime = true
		}
	}
	if !foundRuntime {
		t.Fatalf("Kubernetes evaluation run = %+v", record.Runs)
	}
}

// TestEvalBaseline runs the selected live gate against both supported providers and files
// each provider's capture under artifacts/eval. Gated: it spends real money and needs
// credentials, so it runs only when asked —
//
//	OC_EVAL=1 OC_EVAL_PROFILE=v0.1-budget OC_EVAL_ANTHROPIC_MODEL=claude-haiku-4-5 \
//	OC_EVAL_ANTHROPIC_KEY_FILE=/path/to/anthropic-key \
//	OC_EVAL_ZAI_MODEL=glm-4.7 OC_EVAL_ZAI_KEY_FILE=/path/to/zai-key \
//	go test -run TestEvalBaseline -timeout 4h ./test/controlplane
//
// OC_EVAL_LABEL names the capture (default baseline-1-current); OC_EVAL_JUDGE=1 adds
// the rubric layer. OC_EVAL_PROVIDER selects one provider only when resuming a timed-out
// capture; OC_EVAL_PROFILE defaults to v0.1-budget and only the explicit value exhaustive
// selects the costly baseline. Release evidence requires a passing budget artifact from each provider.
func TestEvalBaseline(t *testing.T) {
	if os.Getenv("OC_EVAL") != "1" {
		t.Skip("set OC_EVAL=1 with Anthropic and Z.AI model credentials to run the live release gate")
	}
	label := os.Getenv("OC_EVAL_LABEL")
	if label == "" {
		label = "baseline-1-current"
	}
	profile, err := evalProfileNamed(os.Getenv("OC_EVAL_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evalCasesForProfile(profile, evalCases(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range evalModelsFromEnvironment(t) {
		model := model
		t.Run(model.Provider, func(t *testing.T) {
			var rounds [][]evalScore
			var allScores []evalScore
			var records []evalRecord
			for attempt := 1; attempt <= profile.runs; attempt++ {
				var scores []evalScore
				for _, one := range cases {
					one := one
					t.Run(fmt.Sprintf("run-%d/%s", attempt, one.Name), func(t *testing.T) {
						record := runEvalCase(t, one, model, nil)
						record.Attempt = attempt
						score := scoreEvalCase(one, record)
						score.Attempt = attempt
						if os.Getenv("OC_EVAL_JUDGE") == "1" {
							score.Judge = judgeOrReport(t, model, one, record)
						}
						scores = append(scores, score)
						records = append(records, record)
					})
				}
				rounds = append(rounds, scores)
				allScores = append(allScores, scores...)
			}
			directory := writeEvalReport(t, filepath.Join("..", "..", "artifacts", "eval"),
				label+"-"+profile.name+"-"+model.Provider, gitRevision(t), evalAgentRevision(t),
				model.Provider+"/"+model.Name, allScores, records)
			if err := validateEvalProfileGate(profile, rounds); err != nil {
				t.Fatalf("%s live release gate: %v; report saved under %s",
					model.Provider, err, directory)
			}
			t.Logf("%s %s evaluation capture filed under %s: %d cases across %d runs",
				model.Provider, profile.name, directory, len(rounds[0]), profile.runs)
		})
	}
}

// evalAgentRevision derives the same agent revision production derives, through the
// same catalog assembly and flattening: the tool declarations do not depend on any
// client, so the set here hashes identically to the running control plane's.
func evalAgentRevision(t *testing.T) string {
	t.Helper()
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(),
		slack.Definition(slack.NewClient(""), nil, false),
		github.Definition(nil, github.NewClient("")),
	)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}
	return reasoning.AgentRevision(catalog.Tools())
}

func evalModelsFromEnvironment(t *testing.T) []evalModel {
	t.Helper()
	specs, err := evalProviderSpecs(os.Getenv("OC_EVAL_PROVIDER"))
	if err != nil {
		t.Fatal(err)
	}
	models := make([]evalModel, 0, len(specs))
	for _, spec := range specs {
		models = append(models, evalModelFromEnvironment(t, spec.prefix, spec.provider))
	}
	return models
}

type evalProviderSpec struct {
	prefix   string
	provider string
}

func evalProviderSpecs(selected string) ([]evalProviderSpec, error) {
	available := []evalProviderSpec{
		{prefix: "ANTHROPIC", provider: "anthropic"},
		{prefix: "ZAI", provider: "zai"},
	}
	selected = strings.TrimSpace(strings.ToLower(selected))
	if selected == "" {
		return available, nil
	}
	for _, spec := range available {
		if spec.provider == selected {
			return []evalProviderSpec{spec}, nil
		}
	}
	return nil, fmt.Errorf("OC_EVAL_PROVIDER must be anthropic or zai, got %q", selected)
}

func evalModelFromEnvironment(t *testing.T, prefix, provider string) evalModel {
	t.Helper()
	name := "OC_EVAL_" + prefix
	model := evalModel{
		Provider: provider,
		Name:     os.Getenv(name + "_MODEL"),
		Effort:   os.Getenv(name + "_EFFORT"),
		BaseURL:  os.Getenv(name + "_BASE_URL"),
	}
	keyFile := os.Getenv(name + "_KEY_FILE")
	if model.Name == "" || keyFile == "" {
		t.Fatalf("OC_EVAL=1 needs %s_MODEL and %s_KEY_FILE", name, name)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("reading the model key file: %v", err)
	}
	model.Key = strings.TrimSpace(string(key))
	return model
}

func judgeOrReport(t *testing.T, model evalModel, one evalCase, record evalRecord) *evalVerdict {
	t.Helper()
	ctx, done := context.WithTimeout(context.Background(), judgeTimeout)
	defer done()
	deployment := reasoning.Deployment{
		Provider:   model.Provider,
		Model:      model.Name,
		Effort:     reasoning.EffortLow,
		BaseURL:    model.BaseURL,
		Credential: reasoning.Secret(model.Key),
	}.WithDefaults()
	verdict, err := judgeEvalCase(ctx, deployment, one, record)
	if err != nil {
		t.Logf("the judge could not grade %s: %v", one.Name, err)
		return nil
	}
	return verdict
}

// gitRevision identifies the capture's code. Best-effort: an artifact without a
// revision is still a capture, and the log says why.
func gitRevision(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func evalCaseNamed(t *testing.T, cases []evalCase, name string) evalCase {
	t.Helper()
	for _, one := range cases {
		if one.Name == name {
			return one
		}
	}
	t.Fatalf("no evaluation case named %q", name)
	return evalCase{}
}

func mustAtoi(t *testing.T, text string) int {
	t.Helper()
	number, err := strconv.Atoi(text)
	if err != nil {
		t.Fatalf("%q is not a number: %v", text, err)
	}
	return number
}

func mustJSON(t *testing.T, payload any) []byte {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding fixture json: %v", err)
	}
	return encoded
}

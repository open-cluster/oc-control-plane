package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// The evaluation harness's two entry points. The deterministic pipeline runs in every
// suite: it proves the worlds serve, the plane investigates them, and the scorer scores
// — with the scripted reasoner, so CI never pays a provider. The capture run is gated:
// it drives the real model over every case and files the scores as an artifact, which is
// how issue #5's baselines are taken.

func TestEvalPipelineDeterministic(t *testing.T) {
	cases := evalCases(time.Now().UTC())

	t.Run("a scripted walk through the single-cause world scores as found", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "single-root-cause")
		reasoner := &scriptedReasoner{decisions: []investigation.Decision{
			{Calls: []investigation.ToolCall{
				{Tool: "slack.list_channels", Arguments: map[string]any{}},
				{Tool: "slack.get_channel_history",
					Arguments: map[string]any{"channel": "C2"}},
			}, Spend: investigation.Spend{InputTokens: 100, OutputTokens: 10, MicroCents: 5}},
			{Calls: []investigation.ToolCall{
				{Tool: "github.list_repositories", Arguments: map[string]any{}},
				{Tool: "github.read_commits",
					Arguments: map[string]any{"repositoryId": float64(101)}},
			}, Spend: investigation.Spend{InputTokens: 120, OutputTokens: 12, MicroCents: 6}},
			{Findings: []investigation.Finding{{
				Statement: "commit abc123 raised the connection pool timeout shortly " +
					"before the alert; the deploy channel announced it going out",
				Sources: []int{2, 4},
			}}, Spend: investigation.Spend{InputTokens: 140, OutputTokens: 20, MicroCents: 8}},
		}}

		record := runEvalCase(t, one, evalModel{}, reasoner)
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
		if score.ToolPrecision != 1 {
			t.Errorf("precision = %v, want 1 for all-relevant calls", score.ToolPrecision)
		}

		directory := writeEvalReport(t, t.TempDir(), "pipeline-proof", "test", "scripted",
			[]evalScore{score}, []evalRecord{record})
		var report evalReport
		raw, err := os.ReadFile(filepath.Join(directory, "report.json"))
		if err != nil {
			t.Fatalf("the report artifact was not written: %v", err)
		}
		if err := json.Unmarshal(raw, &report); err != nil || len(report.Cases) != 1 {
			t.Fatalf("the report artifact does not carry the scores: %v %s", err, raw)
		}
	})

	t.Run("the empty world scores honesty rather than fabrication", func(t *testing.T) {
		one := evalCaseNamed(t, cases, "missing-data-unresolved")
		reasoner := &scriptedReasoner{decisions: []investigation.Decision{
			{Calls: []investigation.ToolCall{
				{Tool: "slack.get_channel_history",
					Arguments: map[string]any{"channel": "C1"}},
			}, Spend: investigation.Spend{InputTokens: 90, OutputTokens: 9, MicroCents: 4}},
			{Spend: investigation.Spend{InputTokens: 95, OutputTokens: 5, MicroCents: 4}},
		}}

		record := runEvalCase(t, one, evalModel{}, reasoner)
		score := scoreEvalCase(one, record)

		if score.Status != "concluded" {
			t.Fatalf("status = %q: %+v", score.Status, record)
		}
		if score.FabricatedFindings != 0 {
			t.Errorf("an empty conclusion scored %d fabricated findings",
				score.FabricatedFindings)
		}
	})
}

// TestEvalBaseline captures one full evaluation run against the real model and files it
// under artifacts/eval. Gated: it spends real money and needs a credential, so it runs
// only when asked —
//
//	OC_EVAL=1 OC_EVAL_MODEL_PROVIDER=anthropic OC_EVAL_MODEL_NAME=claude-sonnet-5 \
//	OC_EVAL_MODEL_KEY_FILE=/path/to/key go test -run TestEvalBaseline -timeout 120m ./cmd/controlplane
//
// OC_EVAL_LABEL names the capture (default baseline-1-current); OC_EVAL_JUDGE=1 adds
// the rubric layer.
func TestEvalBaseline(t *testing.T) {
	if os.Getenv("OC_EVAL") != "1" {
		t.Skip("set OC_EVAL=1 with OC_EVAL_MODEL_PROVIDER/NAME/KEY_FILE to capture an evaluation run")
	}
	model := evalModelFromEnvironment(t)
	label := os.Getenv("OC_EVAL_LABEL")
	if label == "" {
		label = "baseline-1-current"
	}

	var scores []evalScore
	var records []evalRecord
	for _, one := range evalCases(time.Now().UTC()) {
		t.Run(one.Name, func(t *testing.T) {
			record := runEvalCase(t, one, model, nil)
			score := scoreEvalCase(one, record)
			if os.Getenv("OC_EVAL_JUDGE") == "1" {
				score.Judge = judgeOrReport(t, model, one, record)
			}
			scores = append(scores, score)
			records = append(records, record)
		})
	}

	directory := writeEvalReport(t, filepath.Join("..", "..", "artifacts", "eval"),
		label, gitRevision(t), model.Provider+"/"+model.Name, scores, records)
	t.Logf("evaluation capture filed under %s: %d cases", directory, len(scores))
}

func evalModelFromEnvironment(t *testing.T) evalModel {
	t.Helper()
	model := evalModel{
		Provider: os.Getenv("OC_EVAL_MODEL_PROVIDER"),
		Name:     os.Getenv("OC_EVAL_MODEL_NAME"),
		Effort:   os.Getenv("OC_EVAL_MODEL_EFFORT"),
		BaseURL:  os.Getenv("OC_EVAL_MODEL_BASE_URL"),
	}
	keyFile := os.Getenv("OC_EVAL_MODEL_KEY_FILE")
	if model.Provider == "" || model.Name == "" || keyFile == "" {
		t.Fatal("OC_EVAL=1 needs OC_EVAL_MODEL_PROVIDER, OC_EVAL_MODEL_NAME and OC_EVAL_MODEL_KEY_FILE")
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

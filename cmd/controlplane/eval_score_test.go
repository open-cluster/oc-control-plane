package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Scoring one case's record against its ground truth: the programmatic metric layer of
// issue #5 §11. Everything here is mechanical — markers, ordinals and counts — so a
// score never depends on a judgement the rubric layer owns.

// evalScore is one case's programmatic metrics.
type evalScore struct {
	Case   string `json:"case"`
	Status string `json:"status"`

	CausesTotal        int  `json:"causesTotal"`
	CausesFound        int  `json:"causesFound"`
	MultiCauseComplete bool `json:"multiCauseComplete"`

	DiscriminatingTotal int `json:"discriminatingTotal"`
	DiscriminatingMade  int `json:"discriminatingMade"`

	ToolCalls       int     `json:"toolCalls"`
	FailedRuns      int     `json:"failedRuns"`
	TruncatedRuns   int     `json:"truncatedRuns"`
	DuplicateCalls  int     `json:"duplicateCalls"`
	IrrelevantCalls int     `json:"irrelevantCalls"`
	DistractorReads int     `json:"distractorReads"`
	ToolPrecision   float64 `json:"toolPrecision"`
	// ToolRecall is over the DISTINCT tools the discriminating reads name, not the
	// whole relevant set: an economical investigation that read only what separates
	// explanations must not score as under-read.
	ToolRecall float64 `json:"toolRecall"`

	FalseClaims        int `json:"falseClaims"`
	FabricatedFindings int `json:"fabricatedFindings"`

	// Spend's input tokens are the only context measure Baseline 1 can report:
	// per-call context decomposition arrives with the phase-4 telemetry, and the
	// report gains it then rather than faking it now.
	Spend       evalSpend     `json:"spend"`
	WallClock   time.Duration `json:"wallClockNS"`
	Judge       *evalVerdict  `json:"judge,omitempty"`
	FindingText []string      `json:"findingText"`
}

// scoreEvalCase computes the metrics for one record.
func scoreEvalCase(one evalCase, record evalRecord) evalScore {
	score := evalScore{
		Case:                one.Name,
		Status:              record.Status,
		CausesTotal:         len(one.Truth.Causes),
		DiscriminatingTotal: len(one.Truth.Discriminating),
		ToolCalls:           len(record.Runs),
		Spend:               record.Spend,
		WallClock:           record.WallClock,
	}

	for _, cause := range one.Truth.Causes {
		if causeFound(cause, record) {
			score.CausesFound++
		}
	}
	score.MultiCauseComplete = score.CausesFound == score.CausesTotal

	for _, read := range one.Truth.Discriminating {
		if readMade(read, record.Runs) {
			score.DiscriminatingMade++
		}
	}

	relevant := map[string]bool{}
	for _, tool := range one.Truth.RelevantTools {
		relevant[tool] = true
	}
	distractor := map[string]bool{}
	for _, id := range record.DistractorIntegrations {
		distractor[id] = true
	}
	seen := map[string]int{}
	usedRelevant := map[string]bool{}
	relevantCalls := 0
	for _, run := range record.Runs {
		if run.Outcome == "failed" {
			score.FailedRuns++
		}
		if run.Truncated {
			score.TruncatedRuns++
		}
		if distractor[run.IntegrationID] {
			score.DistractorReads++
		}
		if relevant[run.Tool] && !distractor[run.IntegrationID] {
			relevantCalls++
			usedRelevant[run.Tool] = true
		} else {
			score.IrrelevantCalls++
		}
		seen[callIdentity(run)]++
	}
	for _, count := range seen {
		if count > 1 {
			score.DuplicateCalls += count - 1
		}
	}
	if score.ToolCalls > 0 {
		score.ToolPrecision = float64(relevantCalls) / float64(score.ToolCalls)
	}
	essential := map[string]bool{}
	for _, read := range one.Truth.Discriminating {
		essential[read.Tool] = true
	}
	if len(essential) > 0 {
		used := 0
		for tool := range essential {
			if usedRelevant[tool] {
				used++
			}
		}
		score.ToolRecall = float64(used) / float64(len(essential))
	}

	for _, finding := range record.Findings {
		score.FindingText = append(score.FindingText, finding.Statement)
		statement := strings.ToLower(finding.Statement)
		for _, banned := range one.Truth.MustNotClaim {
			if strings.Contains(statement, strings.ToLower(banned)) &&
				!rulesOut(statement) {
				score.FalseClaims++
			}
		}
	}
	if !one.Truth.ExpectFindings {
		score.FabricatedFindings = len(record.Findings)
	}
	return score
}

// rulesOut reports whether a statement mentioning a banned marker is ruling it out
// rather than asserting it — the deceptive-signal archetype rewards exactly that, and
// a scorer that punished "the dns migration was ruled out" would grade the right
// behavior wrong. Heuristic on purpose; the rubric layer owns the judgement call.
func rulesOut(statement string) bool {
	for _, cue := range []string{
		"ruled out", "rules out", "not the cause", "no evidence", "unchanged",
		"unrelated", "did not cause", "not caused",
	} {
		if strings.Contains(statement, cue) {
			return true
		}
	}
	return false
}

// causeFound applies the two-part test: a marker in a finding's statement, and that
// finding citing a run of the cause's tool. A cause asserted without its provenance does
// not count.
func causeFound(cause causeTruth, record evalRecord) bool {
	for _, finding := range record.Findings {
		statement := strings.ToLower(finding.Statement)
		carries := false
		for _, marker := range cause.Markers {
			if strings.Contains(statement, strings.ToLower(marker)) {
				carries = true
				break
			}
		}
		if !carries {
			continue
		}
		for _, ordinal := range finding.Sources {
			if run, found := runAt(record.Runs, ordinal); found && run.Tool == cause.Tool {
				return true
			}
		}
	}
	return false
}

func readMade(read readTruth, runs []evalRun) bool {
	for _, run := range runs {
		if run.Tool != read.Tool {
			continue
		}
		if read.ArgMarker == "" ||
			strings.Contains(renderArguments(run.Arguments), read.ArgMarker) {
			return true
		}
	}
	return false
}

func runAt(runs []evalRun, ordinal int) (evalRun, bool) {
	for _, run := range runs {
		if run.Ordinal == ordinal {
			return run, true
		}
	}
	return evalRun{}, false
}

// callIdentity is a canonical tool-plus-arguments key, so a repeated identical read
// counts as the duplicate it is.
func callIdentity(run evalRun) string {
	return run.Tool + " " + renderArguments(run.Arguments)
}

func renderArguments(arguments map[string]any) string {
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+fmt.Sprintf("%v", arguments[key]))
	}
	return strings.Join(parts, " ")
}

// evalReport is one whole capture: every case's score and the run's identity, written as
// a JSON artifact. Evaluation state lives here — in files — never in the product schema.
type evalReport struct {
	Label      string `json:"label"`
	CapturedAt string `json:"capturedAt"`
	Revision   string `json:"revision"`
	// AgentRevision is the derived hash over what the model saw — preamble, conclusion
	// schema, tool definitions — so any two captures are comparable or visibly not.
	AgentRevision string      `json:"agentRevision"`
	Model         string      `json:"model"`
	Cases         []evalScore `json:"cases"`
}

// writeEvalReport files the report and the raw records under dir, one directory per
// capture, and returns the directory.
func writeEvalReport(
	t *testing.T, dir, label, revision, agentRevision, model string,
	scores []evalScore, records []evalRecord,
) string {
	t.Helper()

	stamp := time.Now().UTC().Format("20060102-150405")
	target := filepath.Join(dir, stamp+"-"+label)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("creating the artifact directory: %v", err)
	}
	report := evalReport{
		Label:         label,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		Revision:      revision,
		AgentRevision: agentRevision,
		Model:         model,
		Cases:         scores,
	}
	writeJSONFile(t, filepath.Join(target, "report.json"), report)
	for index, record := range records {
		writeJSONFile(t, filepath.Join(target,
			"record-"+strconv.Itoa(index+1)+"-"+record.Case+".json"), record)
	}
	return target
}

func writeJSONFile(t *testing.T, path string, payload any) {
	t.Helper()
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

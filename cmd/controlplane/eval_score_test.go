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

	// A conversation-shaped case is scored on its REPLY. These are zero for an incident,
	// which was asked nothing.
	Turns              int  `json:"turns,omitempty"`
	AnswerMarkersTotal int  `json:"answerMarkersTotal,omitempty"`
	AnswerMarkersFound int  `json:"answerMarkersFound,omitempty"`
	SurvivingTotal     int  `json:"survivingTotal,omitempty"`
	SurvivingFound     int  `json:"survivingFound,omitempty"`
	CompactionExpected bool `json:"compactionExpected,omitempty"`
	CompactionHappened bool `json:"compactionHappened,omitempty"`

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
		// A finding whose kind rules an explanation out — or leaves it explicitly open —
		// is not claiming it; only an asserting kind can claim a banned marker.
		if !assertsSomething(finding.Kind) {
			continue
		}
		statement := strings.ToLower(finding.Statement)
		for _, banned := range one.Truth.MustNotClaim {
			if strings.Contains(statement, strings.ToLower(banned)) &&
				!rulesOut(statement) {
				score.FalseClaims++
			}
		}
	}
	if !one.Truth.ExpectFindings {
		for _, finding := range record.Findings {
			// A CAUSE, not merely a statement. An observation of what was searched and
			// not found is the honest negative this world is built to reward.
			if assertsACause(finding.Kind) {
				score.FabricatedFindings++
			}
		}
	}

	scoreConversation(one, record, &score)
	return score
}

// scoreConversation adds the metrics only a question has: whether the reply said what it
// was asked for, and whether what the conversation established at the start was still
// there at the end.
func scoreConversation(one evalCase, record evalRecord, score *evalScore) {
	score.Turns = len(record.Turns)
	score.CompactionExpected = one.Truth.ExpectCompaction
	score.CompactionHappened = record.Compactions > 0

	score.AnswerMarkersTotal = len(one.Truth.AnswerMarkers)
	answer := strings.ToLower(record.Answer)
	for _, marker := range one.Truth.AnswerMarkers {
		// Carrying the marker is not answering with it. "running v2.13.9, not v2.14.1"
		// contains v2.14.1 and asserts its opposite — and v2.13.9 is in that world
		// precisely as the plausible wrong answer, so the case the scorer must catch is
		// the one it used to score full marks.
		if strings.Contains(answer, strings.ToLower(marker)) &&
			!disowns(record.Answer, marker) {
			score.AnswerMarkersFound++
		}
	}

	// The world's own wrong values. Asserting one is a false claim rather than a partial
	// answer: a hedge naming the distractor beside the right answer is not half right,
	// and for a question like ownership it is useless. Naming one to rule it out is not
	// penalised — that is the behaviour the deceptive archetypes reward.
	for _, wrong := range one.Truth.MustNotAnswer {
		if strings.Contains(answer, strings.ToLower(wrong)) &&
			!disowns(record.Answer, wrong) {
			score.FalseClaims++
		}
	}

	// Survival is asked of the LAST turn alone. The union across turns would find every
	// fact in the turn that established it, and score a conversation that forgot
	// everything as one that remembered all of it.
	score.SurvivingTotal = len(one.Truth.Survives)
	if len(one.Truth.Survives) == 0 || len(record.Turns) == 0 {
		return
	}
	last := record.Turns[len(record.Turns)-1]
	recalled := strings.ToLower(last.Answer)
	for _, finding := range last.Findings {
		recalled += " " + strings.ToLower(finding.Statement)
	}
	for _, fact := range one.Truth.Survives {
		if strings.Contains(recalled, strings.ToLower(fact)) {
			score.SurvivingFound++
		}
	}
}

// assertsSomething reports whether a finding's kind claims a positive fact at all. It
// answers the banned-marker question: did this finding STATE the thing it must not?
// Ruling an explanation out and naming an unresolved lead do not. An absent kind (a
// legacy record) still counts as an assertion.
func assertsSomething(kind string) bool {
	return kind != "ruled_out" && kind != "unresolved_lead"
}

// assertsACause reports whether a finding's kind claims an EXPLANATION — which is a
// different question from whether it states a fact, and the one an empty world asks.
//
// The two were one predicate, and that was the defect. On a world where nothing
// happened, "the batch repository has no commits in the window" is an `observation`: it
// states a fact, so assertsSomething is true of it, and it was counted as a fabricated
// finding. It is the opposite — stating what was looked for and not found is exactly
// what that world rewards, and the release capture lost three marks to it.
//
// The list is an ALLOWLIST on purpose. A denylist of two made every future kind
// fabrication by default, which is how `observation` arrived and was punished the day it
// shipped. A new kind now fails safe, and the frozen-vocabulary gate is what forces
// someone to decide which side it belongs on.
func assertsACause(kind string) bool {
	switch kind {
	case "probable_cause", "contributing_factor", "triggering_change":
		return true
	case "":
		// Concluded before the vocabulary existed, so nothing distinguishes it from a
		// cause and the safe reading is that it is one.
		return true
	default:
		return false
	}
}

// negationCues are the ways a statement disowns a value standing next to it. Deliberately
// spaced or apostrophised so that "another", "nothing" and "notable" do not read as "not".
var negationCues = []string{
	" not ", " no ", "n't ", "n't,", "n't.", " never ", "rather than", "instead of",
	"ruled out", "rules out", "no longer", "not the", "is not", "was not",
	// Supersession is disowning too, and this half was learned the hard way: the guard
	// first flagged "v2.14.1 … superseding v2.13.9" as asserting the wrong revision,
	// when naming what a value REPLACED is a better answer than omitting it. A guard
	// that punishes the fuller correct answer is worse than the gap it closes.
	"supersed", "replac", "previous", "earlier", "prior", "used to", "up from",
}

// disowns reports whether every occurrence of needle in text sits beside a negation. It
// is what separates "running v2.14.1" from "running v2.13.9, not v2.14.1" — the same
// substring, opposite claims — and the window looks BOTH ways because English negates on
// either side: "not v2.14.1" and "v2.14.1 is not the owner".
//
// Heuristic, like rulesOut, and for the same reason: the rubric layer owns judgement.
// What this owns is refusing to score a contradiction as an answer.
func disowns(text, needle string) bool {
	text, needle = strings.ToLower(text), strings.ToLower(needle)
	const window = 40

	found := false
	for offset := 0; ; {
		index := strings.Index(text[offset:], needle)
		if index < 0 {
			break
		}
		found = true
		at := offset + index
		start := max(0, at-window)
		end := min(len(text), at+len(needle)+window)
		if !containsAny(text[start:end], negationCues) {
			// One plain mention is enough: an answer that states the value once
			// without disowning it has answered with it, whatever else it says.
			return false
		}
		offset = at + len(needle)
	}
	return found
}

func containsAny(text string, cues []string) bool {
	for _, cue := range cues {
		if strings.Contains(text, cue) {
			return true
		}
	}
	return false
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
// finding citing a run of one of the cause's evidence tools. A cause asserted without
// its provenance does not count.
func causeFound(cause causeTruth, record evalRecord) bool {
	evidence := map[string]bool{}
	for _, tool := range cause.Tools {
		evidence[tool] = true
	}
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
			if run, found := runAt(record.Runs, ordinal); found && evidence[run.Tool] {
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

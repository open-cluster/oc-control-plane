package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE CONVERSATION WORLDS, DRIVEN. These run in every suite with a scripted exchange, so
// CI never pays a provider, and they prove the same three things the gated capture proves
// with a real model: a question is asked through the real surface, a follow-up is a turn
// against the same conversation, and what the conversation established at the start is
// still there after it has been compacted.

// A PEACETIME QUESTION, END TO END. No alert, no episode: a person asks something and the
// plane answers it from what it can read.
func TestAPeacetimeQuestionIsAnsweredFromWhatWasRead(t *testing.T) {
	one := evalCaseNamed(t, evalCases(time.Now().UTC()),
		"peacetime-which-revision-is-deployed")

	exchange := &scriptedExchangeMain{moves: []investigation.Move{
		{Calls: []investigation.AgentCall{
			{ID: "c1", Tool: "slack.list_channels", Arguments: map[string]any{}},
			{ID: "c2", Tool: "slack.get_channel_history",
				Arguments: map[string]any{"channel": "C2"}},
		}, Spend: investigation.Spend{InputTokens: 100, OutputTokens: 10, MicroCents: 5}},
		{Conclusion: &investigation.Conclusion{
			Answer: "payments is running v2.14.1 in production; it was deployed 20 " +
				"minutes ago, superseding v2.13.9.",
			Findings: []investigation.Finding{{
				Statement:  "the deploys channel announced payments v2.14.1 to production",
				Kind:       investigation.FindingObservation,
				Confidence: investigation.ConfidenceConfirmed,
				Sources:    []int{2},
			}},
		}, Spend: investigation.Spend{InputTokens: 120, OutputTokens: 20, MicroCents: 8}},
	}}

	record := runEvalCase(t, one, evalModel{},
		&scriptedInvestigatorMain{exchange: exchange})
	score := scoreEvalCase(one, record)

	if score.Status != "concluded" {
		t.Fatalf("status = %q: %+v", score.Status, record)
	}
	if len(record.Turns) != 1 {
		t.Fatalf("turns = %d, want the one question that was asked", len(record.Turns))
	}
	if score.AnswerMarkersFound != score.AnswerMarkersTotal || score.AnswerMarkersTotal == 0 {
		t.Errorf("answer markers %d/%d; the reply is the deliverable of a question",
			score.AnswerMarkersFound, score.AnswerMarkersTotal)
	}
	if score.DiscriminatingMade != score.DiscriminatingTotal {
		t.Errorf("discriminating reads %d/%d; the answer depends on the deploy channel",
			score.DiscriminatingMade, score.DiscriminatingTotal)
	}
	// The finding states a fact with no causal role, which is what the observation kind
	// exists for. Scoring it as a fabrication would grade the right behaviour wrong.
	if score.FabricatedFindings != 0 || score.FalseClaims != 0 {
		t.Errorf("fabrications=%d falseClaims=%d on an answered question",
			score.FabricatedFindings, score.FalseClaims)
	}
	// The distractor is CONNECTED, and reads against it are attributed to it. A script
	// names a tool rather than a source, so the plane fans that tool across every Slack
	// integration and a scripted run always touches the marketing workspace. What this
	// pins is the attribution: those reads land in the distractor count and out of tool
	// precision, which is the measure the real-model capture is then graded on.
	if score.DistractorReads == 0 {
		t.Errorf("no read was attributed to the connected distractor; the campaigns "+
			"workspace is what tool precision is measured against: %+v", record)
	}
	if score.ToolPrecision >= 1 {
		t.Errorf("tool precision = %v with a distractor read in the record",
			score.ToolPrecision)
	}
}

// MEMORY ACROSS COMPACTION, ASSERTED WHERE IT IS REAL.
//
// The assertion is on the ORIENTATION the last turn was handed, not on what the script
// then said. The plane built that brief from the database after compacting it, so it is
// production behaviour; asserting on a scripted answer would be asserting on the script.
func TestAFactEstablishedFirstSurvivesToTheEndOfALongConversation(t *testing.T) {
	one := evalCaseNamed(t, evalCases(time.Now().UTC()),
		"conversation-memory-across-compaction")

	// Turn one establishes the fact the whole case is about, from the deploy channel.
	established := &memoryScript{
		moves: []investigation.Move{
			{Calls: []investigation.AgentCall{
				{ID: "c1", Tool: "slack.list_channels", Arguments: map[string]any{}},
				{ID: "c2", Tool: "slack.get_channel_history",
					Arguments: map[string]any{"channel": "C2"}},
			}},
		},
		conclusion: investigation.Conclusion{
			Answer: "commit abc123 was deployed to payments shortly before the latency " +
				"rose, raising the connection pool timeout from 2s to 30s.",
			Findings: []investigation.Finding{{
				Statement: "the deploys channel records commit abc123 going to " +
					"production, raising the payments connection pool timeout from 2s " +
					"to 30s",
				Kind:       investigation.FindingTriggeringChange,
				Confidence: investigation.ConfidenceConfirmed,
				Sources:    []int{2},
			}},
			NextSteps: []string{"confirm the pool timeout against the running config"},
		},
	}
	// The reads are on the early turns, while the thread is still short enough for a turn
	// to have room to make them. Everything after answers from memory alone, which is what
	// this case exists to measure.
	recorder := &briefRecorder{scripts: []investigation.Exchange{
		established,
		observing("the deploy at 14:02 precedes the first elevated p99 sample",
			"the cache warmers restarted after the payments deploy, and repopulate over "+
				"several minutes during which downstream reads fall through"),
		answering("the deploy at 14:02 precedes the first elevated p99 sample at 14:04, " +
			"so the ordering holds rather than merely the recency."),
		answering("staging did not reproduce it; that is an absence of a reproduction " +
			"rather than evidence the change is safe."),
		answering("the warmers restarted with the deploy, so part of the early window " +
			"is warm-up rather than the pool change."),
		answering("understood: a written diagnosis, no rollback tonight."),
		answering("we established that commit abc123 raised the connection pool timeout."),
	}}

	record := runEvalCase(t, one, evalModel{}, recorder)
	score := scoreEvalCase(one, record)

	if score.Status != "concluded" {
		t.Fatalf("status = %q: %+v", score.Status, record)
	}
	if len(record.Turns) != 7 {
		t.Fatalf("turns = %d, want the question and its six follow-ups: %+v",
			len(record.Turns), record.Turns)
	}
	for _, turn := range record.Turns {
		if turn.Status != "concluded" {
			t.Fatalf("turn %d ended %q; a squeezed budget must force an honest "+
				"conclusion, never a failure: %+v", turn.Turn, turn.Status, record)
		}
	}
	// The world exists to force this. A case that never compacted would pass every
	// assertion below for the uninteresting reason that nothing was ever summarised.
	if !score.CompactionHappened {
		t.Fatalf("the conversation never compacted; this world's whole point is that it "+
			"must, and the assertions after this one would then prove nothing: %+v", record)
	}

	last, found := recorder.orientationFor(len(record.Turns))
	if !found {
		t.Fatalf("the last turn was never opened; %d were", len(recorder.orientations))
	}
	if last.Brief == nil {
		t.Fatal("the last turn was handed no brief at all")
	}

	// THE FACT. Established on turn one, five turns and at least one compaction ago.
	if !briefCarries(last.Brief, "abc123") {
		t.Errorf("the brief handed to the last turn has lost commit abc123: %+v",
			last.Brief)
	}
	// And it survived INSIDE the summary rather than only in the verbatim tail, which is
	// what shows the compaction layer carried it rather than the recency window.
	if !summaryCarries(last.Brief.Summary, "abc123") {
		t.Errorf("commit abc123 is not in the summary the compaction produced: %+v",
			last.Brief.Summary)
	}

	// THE CONSTRAINT, in the operator's own words. An instruction given on turn two holds
	// for the rest of the conversation, and a paraphrase is an instruction nobody gave.
	if !briefCarries(last.Brief, "ignore the database for now, stay on deployments") {
		t.Errorf("the operator's instruction did not survive: %+v", last.Brief)
	}
}

// memoryScript is a scripted exchange that CONCLUDES when it is told to. This world
// deliberately squeezes the context budget until that instruction arrives, and a script
// that carried on proposing reads would fail the turn for a reason no model would ever
// produce — a real one, told to conclude, concludes.
type memoryScript struct {
	mu         sync.Mutex
	moves      []investigation.Move
	conclusion investigation.Conclusion
}

func (m *memoryScript) Next(
	_ context.Context, _ []investigation.CallResult, mustConclude bool, _ string,
) (investigation.Move, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mustConclude || len(m.moves) == 0 {
		return investigation.Move{Conclusion: &m.conclusion}, nil
	}
	next := m.moves[0]
	m.moves = m.moves[1:]
	return next, nil
}

// answering is a turn that reads nothing and replies. What it can say is what the brief
// still carries.
func answering(answer string) *memoryScript {
	return &memoryScript{conclusion: investigation.Conclusion{Answer: answer}}
}

// observing is a turn that reads the deploy channel and states what it found there.
// Findings accumulate across turns and are most of what a long conversation's held context
// IS, so these are what push it past the budget.
//
// The read is not decoration. A finding must cite the run behind it, so a turn that read
// nothing cannot state one — which is the citation invariant doing exactly its job, and
// the reason this helper reads before it speaks.
func observing(statements ...string) *memoryScript {
	findings := make([]investigation.Finding, 0, len(statements))
	for _, statement := range statements {
		findings = append(findings, investigation.Finding{
			Statement:  statement,
			Kind:       investigation.FindingObservation,
			Confidence: investigation.ConfidenceLikely,
			Sources:    []int{1},
		})
	}
	return &memoryScript{
		moves: []investigation.Move{{Calls: []investigation.AgentCall{{
			ID: "read", Tool: "slack.get_channel_history",
			Arguments: map[string]any{"channel": "C2"},
		}}}},
		conclusion: investigation.Conclusion{
			Answer: statements[0], Findings: findings,
		},
	}
}

// briefCarries reports whether some text is anywhere the last turn could read it.
func briefCarries(brief *investigation.Brief, text string) bool {
	if summaryCarries(brief.Summary, text) {
		return true
	}
	for _, message := range brief.Recent {
		if strings.Contains(strings.ToLower(message.Text), strings.ToLower(text)) {
			return true
		}
	}
	for _, finding := range brief.Findings {
		if strings.Contains(strings.ToLower(finding.Statement), strings.ToLower(text)) {
			return true
		}
	}
	return false
}

func summaryCarries(summary investigation.Summary, text string) bool {
	folded := strings.ToLower(text)
	for _, constraint := range summary.Constraints {
		if strings.Contains(strings.ToLower(constraint), folded) {
			return true
		}
	}
	for _, group := range [][]investigation.PriorFinding{
		summary.Established, summary.RuledOut, summary.Open,
	} {
		for _, finding := range group {
			if strings.Contains(strings.ToLower(finding.Statement), folded) {
				return true
			}
		}
	}
	for _, step := range summary.Recommended {
		if strings.Contains(strings.ToLower(step), folded) {
			return true
		}
	}
	return false
}

package investigation

import (
	"strconv"
	"strings"
	"testing"
)

// COMPACTION, ASSERTED DETERMINISTICALLY.
//
// A conversation that has run for hours must still remember what it was told at the start.
// These do not score a summary — they check, field by field, that the things a long
// conversation cannot afford to lose are still there, and that nothing appeared that was
// never established.

// aBrief is a conversation several turns in: a stated constraint, findings of every kind,
// a failed read and some identifiers.
func aBrief() Brief {
	return Brief{
		ConversationID: "c-1",
		Subject:        "checkout latency",
		Turn:           4,
		Recent: []BriefMessage{
			{FromPerson: true, Actor: "Ada", Text: "ignore the database, look at deployments"},
			{FromPerson: false, Actor: "OpenCluster", Text: "the deploy at 14:02 is the change"},
			{FromPerson: true, Actor: "Ada", Text: "what contradicts the cache hypothesis?"},
		},
		RecentFrom: 7,
		Findings: []PriorFinding{
			{Turn: 1, Statement: "the deploy at 14:02 changed the pool size",
				Kind: FindingTriggeringChange, Confidence: ConfidenceConfirmed,
				Runs: []int{2, 3}},
			{Turn: 2, Statement: "the database was not saturated",
				Kind: FindingRuledOut, Confidence: ConfidenceConfirmed, Runs: []int{1}},
			{Turn: 3, Statement: "whether the cache warmed is unknown",
				Kind: FindingUnresolvedLead, Confidence: ConfidencePossible, Runs: []int{4}},
			{Turn: 3, Statement: "the deployed revision is v2.14.1",
				Kind: FindingObservation, Confidence: ConfidenceConfirmed, Runs: []int{5}},
		},
		FailedReads: []string{"the metrics endpoint returned 503"},
		Identifiers: []string{"C0DEPLOYS", "octo/checkout-api"},
	}
}

// Everything a long conversation cannot afford to lose survives one compaction: the
// operator's own instruction, the established facts with their citations, what was ruled
// out, and what is still open.
func TestCompactionKeepsConstraintsFindingsAndCitations(t *testing.T) {
	t.Parallel()

	summary := compact(aBrief(), 6)

	if summary.Version != 1 {
		t.Errorf("version = %d, want 1", summary.Version)
	}
	if summary.CoversThrough != 6 {
		t.Errorf("coversThrough = %d, want 6", summary.CoversThrough)
	}
	if summary.Goal != "checkout latency" {
		t.Errorf("goal = %q", summary.Goal)
	}

	// THE CONSTRAINT, VERBATIM. A paraphrase is an instruction somebody did not give.
	if !containsExactly(summary.Constraints, "ignore the database, look at deployments") {
		t.Errorf("constraints = %+v; the operator's own words must survive unchanged",
			summary.Constraints)
	}
	// What the agent said is not a constraint. Its findings are kept, with citations; the
	// prose it wrapped them in is not evidence.
	for _, constraint := range summary.Constraints {
		if strings.Contains(constraint, "the deploy at 14:02 is the change") {
			t.Errorf("the agent's own words were kept as an operator instruction: %q",
				constraint)
		}
	}

	if len(summary.Established) != 2 {
		t.Fatalf("established = %+v, want the triggering change and the observation",
			summary.Established)
	}
	for _, finding := range summary.Established {
		if len(finding.Runs) == 0 || finding.Turn == 0 {
			t.Errorf("finding %+v lost its citation; an established fact that cannot be "+
				"followed back to a read is exactly what the citation invariant forbids",
				finding)
		}
	}
	if summary.Established[0].Reference() != "turn 1 run 2, 3" {
		t.Errorf("reference = %q, want the turn and the run ordinals",
			summary.Established[0].Reference())
	}

	// A hypothesis that was ruled out stays ruled out; a question that is open stays open.
	if len(summary.RuledOut) != 1 ||
		summary.RuledOut[0].Statement != "the database was not saturated" {
		t.Errorf("ruledOut = %+v; a dead end that is forgotten is one the agent walks "+
			"back into after ten turns", summary.RuledOut)
	}
	if len(summary.Open) != 1 ||
		summary.Open[0].Statement != "whether the cache warmed is unknown" {
		t.Errorf("open = %+v", summary.Open)
	}

	if !containsExactly(summary.FailedReads, "the metrics endpoint returned 503") {
		t.Errorf("failedReads = %+v; a gap that is explained must not become silent",
			summary.FailedReads)
	}
	if !containsExactly(summary.Identifiers, "octo/checkout-api") {
		t.Errorf("identifiers = %+v", summary.Identifiers)
	}
}

// NOTHING IS INVENTED. Every statement in a summary was in a finding the conversation
// already had, with the citation it already carried. A compaction that could add a claim
// would be a compaction that could manufacture evidence.
func TestCompactionStatesNothingThatWasNotAlreadyEstablished(t *testing.T) {
	t.Parallel()

	brief := aBrief()
	summary := compact(brief, 6)

	established := map[string][]int{}
	for _, finding := range brief.Findings {
		established[finding.Statement] = finding.Runs
	}

	for _, group := range [][]PriorFinding{
		summary.Established, summary.RuledOut, summary.Open,
	} {
		for _, finding := range group {
			runs, known := established[finding.Statement]
			if !known {
				t.Errorf("the summary states %q, which no finding established",
					finding.Statement)
				continue
			}
			if len(runs) != len(finding.Runs) {
				t.Errorf("%q cites %v; the finding cited %v", finding.Statement,
					finding.Runs, runs)
			}
		}
	}
}

// Compaction FOLDS the previous summary in rather than stacking one beside it, and does
// not accumulate the same fact once per round. A conversation that compacts ten times must
// not carry ten copies of turn one.
func TestRepeatedCompactionFoldsRatherThanStacks(t *testing.T) {
	t.Parallel()

	brief := aBrief()
	first := compact(brief, 6)

	// The next round sees the summary it just produced, plus the same turns again — which
	// is exactly what the brief will look like, because the findings are still there.
	brief.Summary = first
	second := compact(brief, 9)

	if second.Version != 2 {
		t.Errorf("version = %d, want 2", second.Version)
	}
	if len(second.Established) != len(first.Established) {
		t.Errorf("established grew from %d to %d across a compaction that added nothing",
			len(first.Established), len(second.Established))
	}
	if len(second.Constraints) != len(first.Constraints) {
		t.Errorf("constraints grew from %d to %d", len(first.Constraints),
			len(second.Constraints))
	}
	if len(second.RuledOut) != 1 || len(second.Open) != 1 {
		t.Errorf("ruledOut=%d open=%d after folding; the kinds must not migrate",
			len(second.RuledOut), len(second.Open))
	}
}

// The earliest instruction is the LAST to be dropped when there are too many. An
// instruction given at the start of an incident holds for the whole of it.
func TestTheOldestConstraintSurvivesTheNewest(t *testing.T) {
	t.Parallel()

	brief := Brief{Subject: "checkout latency"}
	for position := range BriefMaxConstraints + 5 {
		brief.Recent = append(brief.Recent, BriefMessage{
			FromPerson: true, Text: "constraint " + strconv.Itoa(position),
		})
	}

	summary := compact(brief, 1)

	if len(summary.Constraints) != BriefMaxConstraints {
		t.Fatalf("%d constraints kept, want the bound of %d", len(summary.Constraints),
			BriefMaxConstraints)
	}
	if summary.Constraints[0] != "constraint 0" {
		t.Errorf("the first constraint kept is %q; the earliest instruction must be the "+
			"last to go", summary.Constraints[0])
	}
}

// Compaction shrinks what a turn carries. If it did not, the mechanism would be doing
// nothing but adding a table.
func TestCompactionReducesWhatATurnCarries(t *testing.T) {
	t.Parallel()

	brief := aBrief()
	// A conversation with a long tail of messages, which is what actually grows.
	for position := range 40 {
		brief.Recent = append(brief.Recent, BriefMessage{
			FromPerson: position%2 == 0,
			Text:       strings.Repeat("a message with some length to it. ", 10),
		})
	}

	before := briefTokens(brief)
	compacted := Brief{
		Subject: brief.Subject,
		Turn:    brief.Turn,
		Summary: compact(brief, brief.RecentFrom-1),
		Recent:  brief.Recent[len(brief.Recent)-BriefRecentMessages:],
	}
	after := briefTokens(compacted)

	if after >= before {
		t.Errorf("compaction took %d tokens to %d; it must reduce what a turn carries",
			before, after)
	}
}

func containsExactly(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

package e2e

import (
	"strings"
	"testing"
	"time"
)

// The kill criterion, and the ways a set could be made to look like it passed.

func scored(runID, scorer string, conclusion ConclusionVerdict) Score {
	return Score{
		RunID: runID, Scorer: scorer, Conclusion: conclusion,
		ConclusionReason: "because of what the artifact showed",
		Time:             TimeSaved, TimeReason: "it named the failing container immediately",
		Selection: []RoundSelection{
			{Round: 1, Verdict: SelectionAsExpected, Reason: "the reads I would have made"},
		},
	}
}

func truthFor(runID, scenarioID string) GroundTruthRecord {
	return GroundTruthRecord{
		Schema: artifactSchema, RunID: runID, ScenarioID: scenarioID,
		Cause: "something", Expected: ExpectExplanation.String(), RecordedAt: time.Now(),
	}
}

// One occurrence fails the set. It is not averaged, weighted, or traded against successes
// elsewhere, and a set of nine correct answers and one confident wrong one does not pass.
func TestOneWrongAndConfidentAnswerFailsTheWholeSet(t *testing.T) {
	var truth []GroundTruthRecord
	var scores []Score
	for index := range 9 {
		runID := string(rune('a'+index)) + "-run"
		truth = append(truth, truthFor(runID, "scenario-"+string(rune('a'+index))))
		scores = append(scores,
			scored(runID, "first", ConclusionCorrectAndSupported),
			scored(runID, "second", ConclusionCorrectAndSupported))
	}
	truth = append(truth, truthFor("j-run", "red-herring"))
	scores = append(scores,
		scored("j-run", "first", ConclusionWrongAndConfident),
		scored("j-run", "second", ConclusionCorrectAndSupported))

	result := JoinScores(truth, scores)

	if result.Passed {
		t.Fatal("nine correct answers and one wrong-and-confident one passed the set")
	}
	if len(result.FailedBecause) != 1 ||
		!strings.Contains(result.FailedBecause[0], "red-herring") {
		t.Errorf("the failure does not name the run that caused it: %v", result.FailedBecause)
	}
}

// The criterion does not depend on WHICH scorer reached the fatal verdict, or on what the other
// one said about anything else.
//
// This is the shape the bug took: counting "would have saved me time" stopped reading a run's
// scores as soon as one scorer said yes, so a second scorer's wrong-and-confident verdict was
// never seen and the set passed. It is exactly the trade the criterion forbids — a success
// elsewhere silencing the one verdict that is not tradeable — and it hid behind an earlier test
// whose fatal scorer happened to be listed first.
func TestTheKillCriterionDoesNotDependOnScorerOrder(t *testing.T) {
	for _, order := range []struct {
		name  string
		first ConclusionVerdict
		then  ConclusionVerdict
	}{
		{"fatal first", ConclusionWrongAndConfident, ConclusionCorrectAndSupported},
		{"fatal second", ConclusionCorrectAndSupported, ConclusionWrongAndConfident},
	} {
		t.Run(order.name, func(t *testing.T) {
			// Both scorers say it would have saved them time, which is the judgement that must not
			// buy a wrong and confident answer any leniency.
			result := JoinScores(
				[]GroundTruthRecord{truthFor("run-1", "red-herring")},
				[]Score{
					scored("run-1", "first", order.first),
					scored("run-1", "second", order.then),
				})

			if result.Passed {
				t.Fatal("a wrong and confident answer did not fail the set")
			}
			if !result.Runs[0].Fatal {
				t.Error("the run was not marked fatal")
			}
		})
	}
}

// Two independent judgements per run, or the run is not judged. One score cannot produce the
// disagreement the instrument treats as data, and a set passed on half the judgements it asked
// for is a set nobody actually evaluated.
func TestARunWithOnlyOneScoreIsIncomplete(t *testing.T) {
	result := JoinScores(
		[]GroundTruthRecord{truthFor("run-1", "image-pull-failure")},
		[]Score{scored("run-1", "only-one", ConclusionCorrectAndSupported)})

	if result.Passed {
		t.Fatal("a run judged by one scorer passed the set")
	}
	if len(result.Incomplete) != 1 {
		t.Errorf("incomplete = %v, want the singly-scored run", result.Incomplete)
	}
	if !strings.Contains(result.Runs[0].Unscored, "two") {
		t.Errorf("the reason does not say two scorers are required: %q", result.Runs[0].Unscored)
	}
}

// A right answer that did not rest on its evidence is a failure of the standard even though the
// answer was right — but it is not the kill criterion, and conflating the two would make the
// criterion unusable.
func TestALuckyGuessIsRecordedWithoutFailingTheSet(t *testing.T) {
	result := JoinScores(
		[]GroundTruthRecord{truthFor("run-1", "image-pull-failure")},
		[]Score{
			scored("run-1", "first", ConclusionCorrectButUnsupported),
			scored("run-1", "second", ConclusionCorrectButUnsupported),
		})

	if !result.Passed {
		t.Error("a lucky guess failed the set; only a wrong and confident answer does that")
	}
	if result.Runs[0].Scores[0].Conclusion != ConclusionCorrectButUnsupported {
		t.Error("the lucky guess was not recorded as one")
	}
}

// Correctly declining to answer is the right behaviour and is scored as one.
func TestACorrectAbstentionPassesTheSet(t *testing.T) {
	result := JoinScores(
		[]GroundTruthRecord{truthFor("run-1", "cause-outside-the-cluster")},
		[]Score{
			scored("run-1", "first", ConclusionCorrectlyAbstained),
			scored("run-1", "second", ConclusionCorrectlyAbstained),
		})
	if !result.Passed {
		t.Error("a correct abstention failed the set; declining to answer is a first-class result")
	}
}

// Disagreement is a finding about the output's clarity, and is recorded rather than resolved by a
// third vote.
func TestDisagreementIsRecordedRatherThanResolved(t *testing.T) {
	result := JoinScores(
		[]GroundTruthRecord{truthFor("run-1", "bad-configmap-value")},
		[]Score{
			scored("run-1", "first", ConclusionCorrectAndSupported),
			scored("run-1", "second", ConclusionCorrectButUnsupported),
		})

	if result.Runs[0].Disagreement == "" {
		t.Fatal("two engineers reached different verdicts and nothing recorded it")
	}
	for _, name := range []string{"first", "second"} {
		if !strings.Contains(result.Runs[0].Disagreement, name) {
			t.Errorf("the disagreement does not name %s: %q", name, result.Runs[0].Disagreement)
		}
	}
	if !result.Passed {
		t.Error("a disagreement failed the set; it is data, not a fatal verdict")
	}
}

// A set is not passed by runs nobody judged. Reporting one as passed would let a set pass by not
// being looked at, which is the cheapest way to make an instrument lie.
func TestARunNobodyScoredMakesTheSetIncompleteRatherThanPassed(t *testing.T) {
	result := JoinScores(
		[]GroundTruthRecord{
			truthFor("run-1", "image-pull-failure"),
			truthFor("run-2", "red-herring"),
		},
		[]Score{
			scored("run-1", "first", ConclusionCorrectAndSupported),
			scored("run-1", "second", ConclusionCorrectAndSupported),
		})

	if result.Passed {
		t.Fatal("a set with an unjudged run was reported as passed")
	}
	if len(result.Incomplete) != 1 || result.Incomplete[0] != "run-2" {
		t.Errorf("incomplete = %v, want the unjudged run", result.Incomplete)
	}
}

// A discarded run is filed rather than dropped, and it makes the set incomplete rather than
// absent: silently missing runs would make a set look complete when it was not.
func TestADiscardedRunIsCarriedIntoTheResult(t *testing.T) {
	discarded := truthFor("run-2", "oom-after-limit-reduction")
	discarded.Discarded = &Discarded{
		Reason: "the container was never OOMKilled", At: time.Now(),
	}

	result := JoinScores(
		[]GroundTruthRecord{truthFor("run-1", "image-pull-failure"), discarded},
		[]Score{scored("run-1", "first", ConclusionCorrectAndSupported)})

	if result.Passed {
		t.Error("a set containing a discarded run was reported as passed")
	}
	var found bool
	for _, run := range result.Runs {
		if run.ScenarioID == "oom-after-limit-reduction" {
			found = true
			if !strings.Contains(run.Unscored, "discarded") {
				t.Errorf("the discarded run does not say it was discarded: %q", run.Unscored)
			}
		}
	}
	if !found {
		t.Error("the discarded run vanished from the result")
	}
}

// Scoring done by someone who built the system is a self-assessment rather than an evaluation, and
// is refused rather than counted.
func TestAScoreFromSomeoneWhoBuiltTheSystemIsRefused(t *testing.T) {
	self := scored("run-1", "the author", ConclusionCorrectAndSupported)
	self.BuiltTheSystem = true

	result := JoinScores([]GroundTruthRecord{truthFor("run-1", "image-pull-failure")},
		[]Score{self})

	if len(result.Refused) != 1 || !strings.Contains(result.Refused[0], "self-assessment") {
		t.Fatalf("a self-assessment was counted: %v", result.Refused)
	}
	if result.Passed {
		t.Error("a set whose only score was refused was reported as passed")
	}
}

// A verdict with no reason is a number nobody can learn from, and the reasons are what turn a
// failing set into a change to make.
func TestAScoreWithoutReasonsIsRefused(t *testing.T) {
	cases := map[string]func(*Score){
		"no conclusion reason": func(s *Score) { s.ConclusionReason = "" },
		"no time verdict":      func(s *Score) { s.Time = "" },
		"no scorer":            func(s *Score) { s.Scorer = "" },
		"a round scored without a reason": func(s *Score) {
			s.Selection = []RoundSelection{{Round: 1, Verdict: SelectionPoor}}
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			score := scored("run-1", "someone", ConclusionCorrectAndSupported)
			spoil(&score)
			if err := score.Valid(); err == nil {
				t.Fatal("an incomplete score was accepted")
			}
		})
	}
}

// Selection is scored per round, so a good first round and a wasted second are visible
// separately. Without that, a change that improves reasoning and degrades selection looks like no
// change at all.
func TestSelectionIsRecordedPerRound(t *testing.T) {
	score := scored("run-1", "someone", ConclusionCorrectAndSupported)
	score.Selection = []RoundSelection{
		{Round: 1, Verdict: SelectionAsExpected, Reason: "exactly the two reads I would have made"},
		{Round: 2, Verdict: SelectionPoor, Reason: "it re-read the workload it already had"},
	}
	if err := score.Valid(); err != nil {
		t.Fatalf("a per-round score was refused: %v", err)
	}

	result := JoinScores([]GroundTruthRecord{truthFor("run-1", "image-pull-failure")},
		[]Score{score})
	rounds := result.Runs[0].Scores[0].Selection
	if len(rounds) != 2 || rounds[1].Verdict != SelectionPoor {
		t.Fatalf("the per-round selection did not survive the join: %+v", rounds)
	}
}

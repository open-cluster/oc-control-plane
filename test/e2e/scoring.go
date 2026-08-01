package e2e

import (
	"fmt"
	"sort"
)

// How a run is judged, and by whom.
//
// Two scores per run, recorded independently, by engineers who did not build the system. Three
// judgements, kept apart because collapsing any two of them loses the thing it was asked for:
//
//   - SELECTION — did it choose the evidence a senior engineer would have chosen, scored per
//     round so a good first round and a wasted second are visible separately.
//   - CONCLUSION — was the terminal outcome right, and was it supported by the evidence cited.
//   - TIME — would this have saved you time. Subjective, and the only question the buyer
//     actually cares about, so it is recorded as its own answer rather than inferred.
//
// The planner both selects evidence and reasons over it, so a bad explanation has two possible
// causes and a single score cannot tell them apart. Without separating them, a change that
// improves reasoning and degrades selection looks like no change at all.

// scorersPerRun is how many independent judgements a run needs before it counts as judged.
//
// Two, and they are recorded independently. Where two engineers disagree about whether an
// explanation was supported, that disagreement is a finding about the output's CLARITY and is
// recorded rather than resolved by a third vote — which is only possible if there are two in the
// first place.
const scorersPerRun = 2

// SelectionVerdict is how one round's evidence requests compare to what a senior engineer would
// have made, GIVEN the brief and what had been returned so far. The qualifier matters: a request
// that looks obvious with hindsight was not necessarily available at the time it was made.
type SelectionVerdict string

const (
	// SelectionAsExpected means these are the reads a senior engineer would have made.
	SelectionAsExpected SelectionVerdict = "as_expected"
	// SelectionPartly means some were right and something obvious was missed or wasted.
	SelectionPartly SelectionVerdict = "partly"
	// SelectionPoor means the round spent its reads on the wrong things.
	SelectionPoor SelectionVerdict = "poor"
)

// ConclusionVerdict is what became of the answer. Six values, and the distinctions between them
// are the whole point: a right answer for the wrong reason is not a success, and declining to
// answer is not a failure.
type ConclusionVerdict string

const (
	// ConclusionCorrectAndSupported is the answer, resting on the evidence it cited.
	ConclusionCorrectAndSupported ConclusionVerdict = "correct_and_supported"
	// ConclusionCorrectButUnsupported is a lucky guess. It counts as a FAILURE of the standard
	// even though the answer was right: a conclusion that does not rest on its evidence will be
	// wrong the next time by the same mechanism that made it right this time.
	ConclusionCorrectButUnsupported ConclusionVerdict = "correct_but_unsupported"
	// ConclusionWrongWithGapSurfaced is a wrong answer that said what it could not check. It is
	// wrong, and it is honestly wrong, which is a different thing to fix.
	ConclusionWrongWithGapSurfaced ConclusionVerdict = "wrong_with_gap_surfaced"
	// ConclusionWrongAndConfident is THE KILL CRITERION. One occurrence across the set fails the
	// set. It is not averaged, weighted, or traded against successes elsewhere.
	ConclusionWrongAndConfident ConclusionVerdict = "wrong_and_confident"
	// ConclusionCorrectlyAbstained is declining to answer when no explanation was supportable.
	// It is the right behaviour and is scored as one.
	ConclusionCorrectlyAbstained ConclusionVerdict = "correctly_abstained"
	// ConclusionWronglyAbstained is declining to answer when the evidence was there.
	ConclusionWronglyAbstained ConclusionVerdict = "wrongly_abstained"
)

// Fatal reports whether this verdict fails the whole set on its own.
func (v ConclusionVerdict) Fatal() bool { return v == ConclusionWrongAndConfident }

// TimeVerdict is the scorer's own answer to whether this would have saved them time, measured
// against what they would otherwise have done.
type TimeVerdict string

const (
	TimeSaved    TimeVerdict = "would_have_saved_time"
	TimeNeutral  TimeVerdict = "made_no_difference"
	TimeCostMore TimeVerdict = "would_have_cost_time"
)

// RoundSelection is one round's selection score.
type RoundSelection struct {
	Round   int              `json:"round"`
	Verdict SelectionVerdict `json:"verdict"`
	// Reason is required by Valid: a verdict with no reason is a number nobody can learn from,
	// and the reasons are what turn a failing set into a change to make.
	Reason string `json:"reason"`
}

// Score is one scorer's judgement of one run, recorded blind.
type Score struct {
	Schema int    `json:"schema"`
	RunID  string `json:"runId"`
	// Scorer identifies who judged it, so two independent judgements can be told apart and so a
	// disagreement can be discussed with the people who disagreed.
	Scorer string `json:"scorer"`
	// BuiltTheSystem must be false for a score to count. A self-assessment is not an evaluation,
	// and the field exists so that the answer is recorded rather than assumed.
	BuiltTheSystem bool              `json:"builtTheSystem"`
	Selection      []RoundSelection  `json:"selection"`
	Conclusion     ConclusionVerdict `json:"conclusion"`
	// ConclusionReason is why, in the scorer's words. For a fatal verdict it is what the next
	// person needs most.
	ConclusionReason string      `json:"conclusionReason"`
	Time             TimeVerdict `json:"wouldHaveSavedTime"`
	TimeReason       string      `json:"wouldHaveSavedTimeReason"`
	Notes            string      `json:"notes,omitempty"`
}

// Valid reports what is wrong with a score, if anything. A malformed score is refused rather than
// counted: a set result computed over judgements that were never actually made is worse than no
// set result, because it looks like evidence.
func (s Score) Valid() error {
	switch {
	case s.RunID == "":
		return fmt.Errorf("a score names no run")
	case s.Scorer == "":
		return fmt.Errorf("run %s: a score names no scorer", s.RunID)
	case s.BuiltTheSystem:
		return fmt.Errorf("run %s: %s built the system, so this is a self-assessment rather "+
			"than an evaluation", s.RunID, s.Scorer)
	case s.Conclusion == "":
		return fmt.Errorf("run %s: %s recorded no conclusion verdict", s.RunID, s.Scorer)
	case s.ConclusionReason == "":
		return fmt.Errorf("run %s: %s recorded a conclusion with no reason", s.RunID, s.Scorer)
	case s.Time == "":
		return fmt.Errorf("run %s: %s did not answer whether it would have saved time",
			s.RunID, s.Scorer)
	}
	for _, round := range s.Selection {
		if round.Verdict == "" || round.Reason == "" {
			return fmt.Errorf("run %s: %s scored round %d without a verdict and a reason",
				s.RunID, s.Scorer, round.Round)
		}
	}
	return nil
}

// RunResult is one run after ground truth has been joined to its scores.
type RunResult struct {
	RunID      string  `json:"runId"`
	ScenarioID string  `json:"scenarioId"`
	Scores     []Score `json:"scores"`
	// Fatal is true when any scorer judged the conclusion wrong and confident.
	Fatal bool `json:"wrongAndConfident"`
	// Disagreement is recorded rather than resolved. Where two independent engineers disagree
	// about whether an explanation was supported, that is a finding about the OUTPUT'S CLARITY,
	// and a third vote would bury it.
	Disagreement string `json:"disagreement,omitempty"`
	// Unscored says why a run contributed no judgement — it was discarded, or nobody scored it.
	Unscored string `json:"unscored,omitempty"`
}

// SetResult is the whole set, joined.
type SetResult struct {
	Schema int         `json:"schema"`
	Runs   []RunResult `json:"runs"`
	// Passed is false if ANY run was judged wrong and confident. It is not a percentage and there
	// is no threshold to tune: one occurrence fails the set.
	Passed bool `json:"passed"`
	// FailedBecause names the runs that failed it, so "the set failed" is never the whole message.
	FailedBecause []string `json:"failedBecause,omitempty"`
	// Incomplete names runs with no valid score. A set is not passed by runs nobody judged, so
	// these are listed and the set is not called passed while any remain.
	Incomplete []string `json:"incomplete,omitempty"`
	// Refused names scores that were not counted, with why.
	Refused []string `json:"refusedScores,omitempty"`
	// TimeSaved is the count of runs at least one scorer said would have saved them time. It is a
	// count rather than a rate because a rate over ten hand-picked scenarios reads as a
	// measurement of the product rather than of the set.
	TimeSaved int `json:"runsAtLeastOneScorerSaidSavedTime"`
}

// JoinScores brings ground truth, artifacts and scores together AFTER the scoring is recorded,
// and applies the kill criterion.
//
// This is the only place the three meet. Joining earlier would put the answer in front of the
// person whose judgement is only worth having without it.
func JoinScores(
	truth []GroundTruthRecord, scores []Score,
) SetResult {
	byRun := map[string][]Score{}
	result := SetResult{Schema: artifactSchema, Passed: true}

	for _, score := range scores {
		if err := score.Valid(); err != nil {
			result.Refused = append(result.Refused, err.Error())
			continue
		}
		byRun[score.RunID] = append(byRun[score.RunID], score)
	}

	sort.Slice(truth, func(i, j int) bool { return truth[i].ScenarioID < truth[j].ScenarioID })
	for _, record := range truth {
		run := RunResult{RunID: record.RunID, ScenarioID: record.ScenarioID}
		run.Scores = byRun[record.RunID]

		switch {
		case record.Discarded != nil:
			run.Unscored = "discarded: " + record.Discarded.Reason
		case len(run.Scores) == 0:
			run.Unscored = "no valid score was recorded for this run"
		case len(run.Scores) < scorersPerRun:
			// One judgement cannot produce the disagreement this instrument treats as data, and a
			// set passed on half the judgements it asked for is one nobody actually evaluated.
			run.Unscored = fmt.Sprintf(
				"only %d of the two independent scores this run needs were recorded",
				len(run.Scores))
		}
		if run.Unscored != "" {
			result.Incomplete = append(result.Incomplete, run.RunID)
			result.Runs = append(result.Runs, run)
			continue
		}

		// EVERY score is read, whatever any other score said. The two loops are separate because
		// they were once one, and counting "would have saved me time" stopped reading the run as
		// soon as a scorer said yes — so a second scorer's wrong-and-confident verdict went unseen
		// and the set passed. That is precisely the trade the criterion forbids: a success
		// elsewhere buying leniency for the one verdict that is not tradeable.
		for _, score := range run.Scores {
			if score.Conclusion.Fatal() {
				run.Fatal = true
			}
		}
		for _, score := range run.Scores {
			if score.Time == TimeSaved {
				result.TimeSaved++
				break
			}
		}
		if run.Fatal {
			result.Passed = false
			result.FailedBecause = append(result.FailedBecause,
				fmt.Sprintf("%s (%s): wrong and confident", record.ScenarioID, run.RunID))
		}
		run.Disagreement = disagreementIn(run.Scores)
		result.Runs = append(result.Runs, run)
	}

	// A set with runs nobody judged is not a passed set. Reporting it as one would let a set
	// pass by not being looked at, which is the cheapest way to make an instrument lie.
	if len(result.Incomplete) > 0 {
		result.Passed = false
	}
	return result
}

// disagreementIn describes where two independent scorers judged the same run differently. It is
// data rather than a problem to resolve: two senior engineers reaching different verdicts about
// whether an explanation was supported is a finding about how clearly it was stated.
func disagreementIn(scores []Score) string {
	if len(scores) < 2 {
		return ""
	}
	first := scores[0]
	for _, other := range scores[1:] {
		if other.Conclusion != first.Conclusion {
			return fmt.Sprintf("%s judged the conclusion %s and %s judged it %s; "+
				"two engineers reading the same artifact disagreed about what it established",
				first.Scorer, first.Conclusion, other.Scorer, other.Conclusion)
		}
	}
	for _, other := range scores[1:] {
		if other.Time != first.Time {
			return fmt.Sprintf("%s and %s agreed on the conclusion and disagreed about whether "+
				"it would have saved them time (%s against %s)",
				first.Scorer, other.Scorer, first.Time, other.Time)
		}
	}
	return ""
}

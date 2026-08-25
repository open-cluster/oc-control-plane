package controlplane

import "testing"

// SCORING A CONVERSATION-SHAPED CASE.
//
// A peacetime question is not scored the way an incident is. There is no cause to name;
// there is a direct answer, and it is either right or it is not. A long conversation adds
// a second mechanical question — did the facts established at the start survive to the
// end — which is asserted here rather than left to a judge, because a judge that is having
// a bad day would score a lost fact as a stylistic quibble.

func TestAPeacetimeAnswerIsScoredOnItsMarkers(t *testing.T) {
	t.Parallel()

	one := evalCase{
		Name:     "which-revision-is-deployed",
		Question: "which revision of checkout-api is running in production?",
		Truth: groundTruth{
			AnswerMarkers:  []string{"v2.14.1"},
			RelevantTools:  []string{"slack.get_channel_history"},
			ExpectFindings: true,
		},
	}
	record := evalRecord{
		Case:   one.Name,
		Status: "concluded",
		Answer: "checkout-api is running v2.14.1 in production.",
		Findings: []evalFinding{{
			Statement: "the deployed revision of checkout-api is v2.14.1",
			Kind:      "observation", Sources: []int{1},
		}},
		Runs: []evalRun{{Ordinal: 1, Tool: "slack.get_channel_history"}},
	}

	score := scoreEvalCase(one, record)

	if score.AnswerMarkersTotal != 1 || score.AnswerMarkersFound != 1 {
		t.Errorf("answer markers %d/%d; the answer carried the revision it was asked for",
			score.AnswerMarkersFound, score.AnswerMarkersTotal)
	}
	// A peacetime case names no causes, and a scorer that reported nought-of-nought found
	// would make an answered question look like a failed investigation.
	if score.CausesTotal != 0 || score.CausesFound != 0 {
		t.Errorf("causes = %d/%d; a question in peacetime has no cause to name",
			score.CausesFound, score.CausesTotal)
	}
}

// An answer that does not carry what it was asked for is not scored on the findings
// beneath it. The prose IS the deliverable for a question, and a correct finding under a
// wrong answer is still a wrong reply.
func TestAnAnswerMissingItsMarkerScoresAsMissing(t *testing.T) {
	t.Parallel()

	one := evalCase{
		Name:     "which-revision-is-deployed",
		Question: "which revision is running?",
		Truth:    groundTruth{AnswerMarkers: []string{"v2.14.1"}, ExpectFindings: true},
	}
	record := evalRecord{
		Status: "concluded",
		Answer: "I could not determine the running revision.",
		Findings: []evalFinding{{
			Statement: "the deployed revision is v2.14.1", Sources: []int{1},
		}},
	}

	score := scoreEvalCase(one, record)

	if score.AnswerMarkersFound != 0 {
		t.Errorf("markers found = %d; the finding is not the answer",
			score.AnswerMarkersFound)
	}
}

// FACT SURVIVAL. A fact established on turn one, asked about again after the conversation
// exceeds the bounded verbatim tail, must still be there. This makes cited-fact loss a
// CI failure instead of a customer's discovery.
func TestASurvivingFactIsFoundInTheLastTurnThatCouldCarryIt(t *testing.T) {
	t.Parallel()

	one := evalCase{
		Name:      "long-conversation",
		Question:  "what changed before the latency rose?",
		FollowUps: []string{"and the cache?", "what did we establish about the deploy?"},
		Truth: groundTruth{
			Survives:       []string{"abc123"},
			ExpectFindings: true,
		},
	}
	record := evalRecord{
		Status: "concluded",
		Turns: []evalTurn{
			{Turn: 1, Answer: "commit abc123 raised the pool timeout."},
			{Turn: 2, Answer: "the cache warmed normally."},
			{Turn: 3, Answer: "we established that commit abc123 raised the pool timeout."},
		},
	}

	score := scoreEvalCase(one, record)

	if score.SurvivingTotal != 1 || score.SurvivingFound != 1 {
		t.Errorf("surviving facts %d/%d; the fact was restated on the final turn",
			score.SurvivingFound, score.SurvivingTotal)
	}
}

// A fact that only the FIRST turn knew is a lost fact, however fluent the last turn was.
// Scoring the union of every turn would find it in turn one and call it survived, which
// is precisely the regression this case exists to catch.
func TestAFactAbsentFromTheFinalFollowUpIsNotSurviving(t *testing.T) {
	t.Parallel()

	one := evalCase{
		Name:      "long-conversation",
		Question:  "what changed?",
		FollowUps: []string{"and the cache?", "what did we establish about the deploy?"},
		Truth:     groundTruth{Survives: []string{"abc123"}},
	}
	record := evalRecord{
		Status: "concluded",
		Turns: []evalTurn{
			{Turn: 1, Answer: "commit abc123 raised the pool timeout."},
			{Turn: 2, Answer: "the cache warmed normally."},
			{Turn: 3, Answer: "I no longer have the detail of what changed."},
		},
	}

	score := scoreEvalCase(one, record)

	if score.SurvivingFound != 0 {
		t.Errorf("surviving = %d; the fact was gone by the turn that was asked for it",
			score.SurvivingFound)
	}
}

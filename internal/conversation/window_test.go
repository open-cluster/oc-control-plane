package conversation

import (
	"testing"
	"time"
)

// THE WINDOW A QUESTION GETS.
//
// A turn attached to an incident anchors its window to when that incident began, and the
// lead widens it backwards so the change that caused it sits inside. A turn with NO
// incident has nothing to anchor to, and used to borrow the same lead as a total width —
// so "what changed recently?" was answered against the last two hours. On 2026-08-22 that
// made a live investigation report a repository as having no commits when it had
// twenty-seven, every one of them older than the window it was never told about.

func TestAQuestionReachesBackAtLeastADay(t *testing.T) {
	t.Parallel()

	if got := QuestionWindow(2 * time.Hour); got < 24*time.Hour {
		t.Errorf("QuestionWindow(2h) = %v; a question with no incident to anchor to "+
			"cannot inherit an incident's lead as its whole width", got)
	}
}

// A deployment that widened the lead meant "look further back". A question must not be
// narrowed below what the operator asked for.
func TestAWiderLeadStillWidensAQuestion(t *testing.T) {
	t.Parallel()

	if got := QuestionWindow(72 * time.Hour); got != 72*time.Hour {
		t.Errorf("QuestionWindow(72h) = %v; a configured lead wider than the floor is "+
			"what the deployment asked for", got)
	}
}

// An unset or nonsensical lead still yields a usable window rather than none at all: a
// zero-width window would make every read empty and every answer a false negative.
func TestAnAbsentLeadStillYieldsAWindow(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{0, -1 * time.Hour} {
		if got := QuestionWindow(lead); got < 24*time.Hour {
			t.Errorf("QuestionWindow(%v) = %v; a window must never collapse", lead, got)
		}
	}
}

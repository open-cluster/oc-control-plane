package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestAnAnswerInsideTheBoundIsUntouched(t *testing.T) {
	t.Parallel()

	answer := "checkout-api is running v2.14.1."
	if got := boundedSummary(answer); got != answer {
		t.Errorf("boundedSummary(%q) = %q; a short summary must be left exactly alone",
			answer, got)
	}
}

func TestABoundedAnswerSaysItWasCut(t *testing.T) {
	t.Parallel()

	got := boundedSummary(strings.Repeat("a", MaxSummaryLength+500))

	if len([]rune(got)) > MaxSummaryLength {
		t.Errorf("boundedAnswer returned %d runes, past the bound of %d",
			len([]rune(got)), MaxSummaryLength)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("a cut answer does not say it was cut, so it reads as a complete "+
			"one; tail = %q", tail(got))
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Error("the answer's own words did not survive the cut")
	}
}

// The mark is only worth its characters if it is inside the bound: appending it after
// truncating to the ceiling would put the result back over.
func TestTheCutMarkIsInsideTheBound(t *testing.T) {
	t.Parallel()

	for _, over := range []int{1, 2, 500, MaxSummaryLength} {
		got := boundedSummary(strings.Repeat("b", MaxSummaryLength+over))
		if len([]rune(got)) > MaxSummaryLength {
			t.Errorf("over by %d: returned %d runes, past the bound of %d",
				over, len([]rune(got)), MaxSummaryLength)
		}
	}
}

func tail(text string) string {
	runes := []rune(text)
	if len(runes) <= 80 {
		return text
	}
	return string(runes[len(runes)-80:])
}

// A READ REPORTS A WINDOW ONLY WHEN IT USED ONE.
//
// Every run carries the window in force, because the record's column is NOT NULL and the
// bound is real. But a repository listing is not filtered by time, and an event that hands
// a reader a window beside it answers "did this read cover my period?" wrongly rather than
// not at all. Only a read that actually filtered by the window reports one.

func TestAnEventReportsNoWindowForAReadThatDidNotUseOne(t *testing.T) {
	t.Parallel()

	payload := investigation.ToolCompletedPayload(ToolRun{
		Ordinal: 1, Tool: "github.list_repositories", Outcome: RunSucceeded,
		Summary: "1 repositories matched",
		// The bound in force, as every run carries — but this read did not filter by it.
		WindowFrom:    time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC),
		WindowUntil:   time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		WindowApplied: false,
	})

	if _, present := payload["windowFrom"]; present {
		t.Errorf("a listing that filtered by no window reports one: %v",
			payload["windowFrom"])
	}
}

func TestAnEventReportsTheWindowForAReadThatUsedOne(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	payload := investigation.ToolCompletedPayload(ToolRun{
		Ordinal: 1, Tool: "github.read_commits", Outcome: RunSucceeded,
		Summary: "0 commits in the window", WindowFrom: from,
		WindowUntil:   time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		WindowApplied: true,
	})

	if payload["windowFrom"] != from.Format(time.RFC3339) {
		t.Errorf("windowFrom = %v; a windowed read must say what it covered",
			payload["windowFrom"])
	}
}

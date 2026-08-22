package investigation

import (
	"strings"
	"testing"
	"time"
)

// THE BOUNDED ANSWER.
//
// The direct answer is bounded where it is persisted, and nowhere else: an over-length
// answer is not malformed, so it must not fail an investigation whose reads all
// succeeded. What a bound must never do is let a cut read as an ending — an answer that
// stops mid-sentence with no mark is indistinguishable from one that finished.

func TestAnAnswerInsideTheBoundIsUntouched(t *testing.T) {
	t.Parallel()

	answer := "checkout-api is running v2.14.1."
	if got := boundedAnswer(answer); got != answer {
		t.Errorf("boundedAnswer(%q) = %q; a short answer must be left exactly alone",
			answer, got)
	}
}

func TestABoundedAnswerSaysItWasCut(t *testing.T) {
	t.Parallel()

	got := boundedAnswer(strings.Repeat("a", MaxAnswerLength+500))

	if len([]rune(got)) > MaxAnswerLength {
		t.Errorf("boundedAnswer returned %d runes, past the bound of %d",
			len([]rune(got)), MaxAnswerLength)
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

	for _, over := range []int{1, 2, 500, MaxAnswerLength} {
		got := boundedAnswer(strings.Repeat("b", MaxAnswerLength+over))
		if len([]rune(got)) > MaxAnswerLength {
			t.Errorf("over by %d: returned %d runes, past the bound of %d",
				over, len([]rune(got)), MaxAnswerLength)
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

// THE WINDOW IN THE EVENT STREAM.
//
// An operator reading the stream is asking the same question the model is: did this read
// cover the period I care about? Answering it from the stream alone is what makes an empty
// result auditable without opening the transcript. The payload is free-form, so this costs
// no migration.

func TestAToolCompletedEventNamesTheWindowTheReadCovered(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	payload := toolCompletedPayload(ToolRun{
		Ordinal: 1, Tool: "github.read_commits", Outcome: RunSucceeded,
		Summary: "0 commits in the window", WindowFrom: from, WindowUntil: until,
	})

	if payload["windowFrom"] != from.Format(time.RFC3339) {
		t.Errorf("windowFrom = %v; a reader cannot tell an empty window from an empty "+
			"estate without it", payload["windowFrom"])
	}
	if payload["windowUntil"] != until.Format(time.RFC3339) {
		t.Errorf("windowUntil = %v", payload["windowUntil"])
	}
}

func TestAToolCompletedEventForAnUnwindowedReadClaimsNoWindow(t *testing.T) {
	t.Parallel()

	payload := toolCompletedPayload(ToolRun{
		Ordinal: 1, Tool: "github.list_repositories", Outcome: RunSucceeded,
		Summary: "1 repositories matched",
	})

	if _, present := payload["windowFrom"]; present {
		t.Errorf("a read that covered no window reports one: %v", payload["windowFrom"])
	}
}

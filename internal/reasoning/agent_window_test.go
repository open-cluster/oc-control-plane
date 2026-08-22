package reasoning

import (
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE WINDOW A READ ACTUALLY COVERED.
//
// Every windowed read is clamped into the investigation's own window, including one the
// model phrased with no window at all. A model that is not told which window it got reads
// an empty result as a fact about the estate rather than about the bounds it was given —
// which is how "no commits in the last two hours" becomes "the repository has no commits".
// The rendered run states the window beside the arguments the model asked with, so a
// narrowing is visible by comparison.

func TestARunStatesTheWindowItActuallyCovered(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	turn := renderResult(investigation.CallResult{
		CallID: "call-1",
		Run: investigation.ToolRun{
			Ordinal:       1,
			Tool:          "github.read_commits",
			Arguments:     map[string]any{"repositoryId": 42},
			Outcome:       investigation.RunSucceeded,
			Summary:       "0 commits",
			WindowFrom:    from,
			WindowUntil:   until,
			WindowApplied: true,
			Content:       []any{},
		},
	})

	if !strings.Contains(turn.Content, stamp(from)) ||
		!strings.Contains(turn.Content, stamp(until)) {
		t.Errorf("the run does not say which window it covered:\n%s", turn.Content)
	}
	if !strings.Contains(turn.Content, "WINDOW:") {
		t.Errorf("the window is not labelled, so it reads as an arbitrary pair of "+
			"timestamps:\n%s", turn.Content)
	}
}

// A read that carries no window of its own — a repository listing, a pull request by
// number — must not grow a window line. Stating a window on a read that has none would
// tell the model its answer was bounded in time when it was not.
func TestARunWithNoWindowStatesNone(t *testing.T) {
	t.Parallel()

	turn := renderResult(investigation.CallResult{
		CallID: "call-1",
		Run: investigation.ToolRun{
			Ordinal:   1,
			Tool:      "github.list_repositories",
			Arguments: map[string]any{},
			Outcome:   investigation.RunSucceeded,
			Summary:   "1 repositories matched",
			Content:   []any{},
		},
	})

	if strings.Contains(turn.Content, "WINDOW:") {
		t.Errorf("a read with no window claims one:\n%s", turn.Content)
	}
}

package agent

import (
	"strings"
	"testing"
)

// Element-aware truncation: a run's list content is cut between elements, never through
// one, so what the model reads is valid items plus an honest count of what it did not
// get — not JSON severed mid-token.

func TestBoundedJSONCutsBetweenElementsAndSaysWhatWasCut(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", maxRunContentBytes/4)
	content := []any{
		map[string]any{"id": 1, "text": big},
		map[string]any{"id": 2, "text": big},
		map[string]any{"id": 3, "text": big},
		map[string]any{"id": 4, "text": big},
		map[string]any{"id": 5, "text": big},
		map[string]any{"id": 6, "text": big},
	}

	rendered := boundedJSON(content)
	if len(rendered) > maxRunContentBytes+256 {
		t.Fatalf("rendered %d bytes, past the budget", len(rendered))
	}
	if !strings.Contains(rendered, `"id":1`) {
		t.Error("the first element did not survive")
	}
	if strings.Contains(rendered, `"id":6`) {
		t.Error("the last element survived a cut that claims a budget")
	}
	if !strings.Contains(rendered, "of 6 items") {
		t.Errorf("the cut does not say what it kept out of what: %s", rendered[len(rendered)-120:])
	}
	// Everything before the cut note is whole elements: the note follows a closed array.
	if !strings.Contains(rendered, "}]") {
		t.Error("the kept elements do not end as a closed JSON array")
	}
}

func TestBoundedJSONLeavesSmallContentAlone(t *testing.T) {
	t.Parallel()

	rendered := boundedJSON([]string{"a", "b"})
	if rendered != `["a","b"]` {
		t.Errorf("rendered = %s", rendered)
	}
}

func TestBoundedJSONStillCutsANonListAtTheByteBudget(t *testing.T) {
	t.Parallel()

	rendered := boundedJSON(map[string]any{"blob": strings.Repeat("y", maxRunContentBytes*2)})
	if len(rendered) > maxRunContentBytes+256 {
		t.Fatalf("rendered %d bytes, past the budget", len(rendered))
	}
	if !strings.Contains(rendered, "cut at") {
		t.Error("a byte cut must say so")
	}
}

package agent

import (
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"strings"
	"testing"
	"time"
)

// INCIDENT TURNS AND QUESTION TURNS.
//
// The preamble used to open by asserting that every turn was one operational incident.
// Conversations made that false: "which revision is deployed?" arrives with no alert, no
// onset and no cause to name, and a model told it is investigating an incident will look
// for one. The answer field and the observation finding kind already exist for exactly
// this; the preamble simply never admitted the second kind of turn existed.

func TestThePreambleDoesNotClaimEveryTurnIsAnIncident(t *testing.T) {
	t.Parallel()

	if strings.Contains(taskInstructions, "working one operational incident") {
		t.Error("the preamble asserts every turn is an incident; a question asked " +
			"outside any incident is then told to investigate one that does not exist")
	}
}

func TestThePreambleNamesBothKindsOfTurn(t *testing.T) {
	t.Parallel()

	lowered := strings.ToLower(taskInstructions)
	for _, needed := range []string{"question", "incident", "observation"} {
		if !strings.Contains(lowered, needed) {
			t.Errorf("the preamble never mentions %q, so a model cannot tell which kind "+
				"of turn it has or what a fact with no causal role is called", needed)
		}
	}
}

// The orientation is what says which kind THIS turn is. A question turn carries the
// operator's words and no triggering alert; leaving that implicit makes the model infer
// from an absence, which is the weakest signal available to it.
func TestAQuestionTurnIsNamedAsOneInTheOrientation(t *testing.T) {
	t.Parallel()

	rendered := renderOrientation(orientation{
		Subject:     "which revision is deployed",
		Question:    "Which revision of checkout-api is running?",
		WindowFrom:  testOrientation().WindowFrom,
		WindowUntil: testOrientation().WindowUntil,
	})

	if !strings.Contains(rendered, "TURN: question") {
		t.Errorf("a turn with a question and no alert does not say so:\n%s", rendered)
	}
}

func testOrientation() orientation {
	return orientation{
		Subject: "checkout latency", Trigger: &investigation.Trigger{Title: "checkout latency"},
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	}
}

func TestAnIncidentTurnIsNamedAsOneInTheOrientation(t *testing.T) {
	t.Parallel()

	rendered := renderOrientation(testOrientation())

	if !strings.Contains(rendered, "TURN: incident") {
		t.Errorf("a turn with a triggering alert does not say so:\n%s", rendered)
	}
}

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

func TestTheBriefKeepsOlderOperatorFactsUntrustedAndPriorLimitationsVisible(t *testing.T) {
	t.Parallel()
	rendered := renderBrief(&investigation.Brief{
		OperatorStatements: []investigation.BriefMessage{{
			FromPerson: true, Actor: "on-call", Text: "traffic stayed flat",
		}},
		Limitations: []string{"database wait telemetry is unavailable"},
	})
	for _, expected := range []string{
		"KNOWN LIMITATIONS", "database wait telemetry is unavailable",
		"OLDER OPERATOR TESTIMONY", "unverified person-authored context",
		"operator on-call: traffic stayed flat",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("brief is missing %q:\n%s", expected, rendered)
		}
	}
}

package reasoning

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
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

	rendered := renderOrientation(investigation.Orientation{
		Subject:     "which revision is deployed",
		Question:    "Which revision of checkout-api is running?",
		WindowFrom:  testOrientation().WindowFrom,
		WindowUntil: testOrientation().WindowUntil,
	})

	if !strings.Contains(rendered, "TURN: question") {
		t.Errorf("a turn with a question and no alert does not say so:\n%s", rendered)
	}
}

func TestAnIncidentTurnIsNamedAsOneInTheOrientation(t *testing.T) {
	t.Parallel()

	rendered := renderOrientation(testOrientation())

	if !strings.Contains(rendered, "TURN: incident") {
		t.Errorf("a turn with a triggering alert does not say so:\n%s", rendered)
	}
}

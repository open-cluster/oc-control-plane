package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (r *Agent) assignedInputFits(state *runState, oriented orientation) (bool, error) {
	candidate := *state
	candidate.task = taskInstruction(oriented)
	candidate.orientationText = renderOrientation(oriented)
	candidate.tools = exchangeTools(oriented)
	encoded, err := json.Marshal(modelPrompt(r, &candidate, false))
	if err != nil {
		return false, err
	}
	// UTF-8 bytes conservatively bound input tokens; reserve output and ten percent headroom.
	return len(encoded) <= state.ceiling-state.ceiling/10, nil
}

func (r *Agent) requestNarrowerInput(ctx context.Context, state *runState, messages []investigation.AssignedMessage) error {
	sequences := make([]int64, len(messages))
	for n, message := range messages {
		sequences[n] = message.Sequence
	}
	const request = "The complete assigned Messages cannot fit within the input budget. Narrow or split the question and send a new Message; none of this batch was answered."
	conclusion := investigation.Conclusion{
		Status: investigation.Inconclusive, Summary: request,
		Impact: investigation.ImpactAssessment{Status: investigation.ImpactUnknown, CurrentState: "unknown",
			Summary: "Input was not processed.", AffectedServices: []string{}, AffectedUsers: []string{}, RunRefs: []int{}},
		Findings: []investigation.Finding{}, Hypotheses: []investigation.HypothesisResult{}, Actions: []investigation.ActionProposal{},
		Limitations: []investigation.Limitation{{Type: investigation.LimitationEssentialHumanInput,
			Statement: request, RunRefs: []int{}, MessageSequences: sequences}},
	}
	writeCtx, done := terminalWriteWindow(ctx)
	defer done()
	if err := r.Store.ConcludeInvestigation(writeCtx, state.organization, state.opened.ID, conclusion,
		investigation.StoppedByContext, state.usage); err != nil {
		return fmt.Errorf("recording unprocessed input: %w", err)
	}
	r.announce(writeCtx, state.events, investigation.EventConcluded, investigation.ConcludedPayload(conclusion, investigation.StoppedByContext))
	return nil
}

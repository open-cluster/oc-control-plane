package agent

import (
	"context"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (r *Agent) initialInputTokens(oriented orientation) (int, error) {
	candidate := runState{task: taskInstruction(oriented),
		orientationText: renderOrientation(oriented), tools: exchangeTools(oriented)}
	return requestTokens(r.model, modelPrompt(r, &candidate, false))
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
	if err := r.Store.ConcludeInvestigation(writeCtx, state.organization, state.opened.ID, state.opened.ClaimToken, conclusion,
		investigation.StoppedByContext, state.usage); err != nil {
		return fmt.Errorf("recording unprocessed input: %w", err)
	}
	return nil
}

package agent

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// MEASURING A TURN'S CONTEXT.
//
// The estimate is characters divided by a constant, and that is the whole of it. A real
// tokenizer would be one dependency per vendor, kept in step with each vendor's releases,
// to produce a number that is then compared against a threshold which already carries a
// safety margin. It is deliberately pessimistic: overestimating ends a turn slightly
// early, while underestimating can exhaust the model's context window.
const charactersPerToken = 2

// EstimateTokens reports the pessimistic token cost of some text.
func EstimateTokens(text string) int {
	return (len(text) + charactersPerToken - 1) / charactersPerToken
}

// briefTokens estimates what a brief will cost a turn. Findings are counted by their
// statements rather than by the evidence behind them, because the evidence is a reference
// and never travels.
func briefTokens(brief Brief) int {
	total := EstimateTokens(brief.Subject)
	for _, message := range brief.Recent {
		total += EstimateTokens(message.Text) + EstimateTokens(message.Actor)
	}
	for _, finding := range brief.Findings {
		total += EstimateTokens(finding.Statement) + EstimateTokens(finding.Reference())
	}
	for _, read := range brief.FailedReads {
		total += EstimateTokens(read)
	}
	for _, step := range brief.Recommended {
		total += EstimateTokens(step)
	}
	for _, identifier := range brief.Identifiers {
		total += EstimateTokens(identifier)
	}
	return total
}

// conversationBrief assembles a bounded message tail and prior cited findings.
// A brief that cannot be read narrows the turn rather than failing it, exactly as the
// trigger and the ledger already do: a follow-up that has lost its memory is worse than one
// that has it, and better than none at all.
func (r *Agent) conversationBrief(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
	_ *investigation.EventStream,
) *Brief {
	if opened.ConversationID == uuid.Nil {
		return nil
	}
	brief, err := r.Store.ConversationBrief(ctx, organization, opened.ConversationID,
		BriefRecentMessages)
	if err != nil {
		r.Logger.Warn("a conversation's brief could not be read; this turn runs without it",
			slog.String("conversation_id", opened.ConversationID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	brief.Turn = opened.Turn
	return &brief
}

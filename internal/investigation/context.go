package investigation

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// MEASURING A TURN'S CONTEXT.
//
// The estimate is characters divided by a constant, and that is the whole of it. A real
// tokenizer would be one dependency per vendor, kept in step with each vendor's releases,
// to produce a number that is then compared against a threshold which already carries a
// safety margin. It is deliberately PESSIMISTIC: being wrong high compacts slightly early,
// being wrong low loses the turn, and those two costs are not the same size.
const charactersPerToken = 2

// EstimateTokens reports the pessimistic token cost of some text.
func EstimateTokens(text string) int {
	return (len(text) + charactersPerToken - 1) / charactersPerToken
}

// briefTokens estimates what a brief will cost a turn. Findings are counted by their
// statements rather than by the evidence behind them, because the evidence is a reference
// and never travels.
func briefTokens(brief Brief) int {
	total := EstimateTokens(brief.Subject) + summaryTokens(brief.Summary)
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

func summaryTokens(summary Summary) int {
	total := EstimateTokens(summary.Goal) + EstimateTokens(summary.Problem)
	for _, constraint := range summary.Constraints {
		total += EstimateTokens(constraint)
	}
	for _, group := range [][]PriorFinding{
		summary.Established, summary.RuledOut, summary.Open,
	} {
		for _, finding := range group {
			total += EstimateTokens(finding.Statement) +
				EstimateTokens(finding.Reference())
		}
	}
	for _, read := range summary.FailedReads {
		total += EstimateTokens(read)
	}
	for _, step := range summary.Recommended {
		total += EstimateTokens(step)
	}
	for _, identifier := range summary.Identifiers {
		total += EstimateTokens(identifier)
	}
	return total
}

// conversationBrief assembles the turn's brief and compacts it if it has outgrown the
// budget.
//
// Compaction happens HERE, at the start of the turn that would otherwise be too big, rather
// than at the end of the one that made it so. The turn about to run is the one that knows
// its own budget, and compacting eagerly at the end of every turn would summarise
// conversations that never needed it.
//
// A brief that cannot be read narrows the turn rather than failing it, exactly as the
// trigger and the ledger already do: a follow-up that has lost its memory is worse than one
// that has it, and better than none at all.
func (r *Runner) conversationBrief(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
	events *stream,
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

	budget := r.ContextBudget
	if budget <= 0 {
		return &brief
	}
	before := briefTokens(brief)
	if before <= budget {
		return &brief
	}

	// Over budget: the older material becomes the next summary version, and what stays
	// verbatim is the recent tail alone.
	through := int64(0)
	if len(brief.Recent) > 0 {
		through = brief.RecentFrom - 1
	}
	next := compact(brief, through)
	compacted := Brief{
		ConversationID: brief.ConversationID,
		Subject:        brief.Subject,
		Turn:           brief.Turn,
		Summary:        next,
		Recent:         brief.Recent,
		RecentFrom:     brief.RecentFrom,
	}
	after := briefTokens(compacted)

	if err := r.Store.RecordConversationSummary(ctx, organization, opened.ConversationID,
		next, before, after, r.ModelName); err != nil {
		// The summary is working memory. Failing to persist it means the next turn
		// recomputes it from the same held facts, which is slower and not wrong.
		r.Logger.Warn("a conversation summary could not be recorded",
			slog.String("conversation_id", opened.ConversationID.String()),
			slog.String("error", err.Error()))
	}
	// The counters and nothing else. What the summary SAYS is internal — a reader learns
	// that memory was consolidated and how much it saved, which is what tells them whether
	// the layer is working.
	r.announce(ctx, events, EventCompacted, compactedPayload(next.Version, before, after))
	r.Telemetry.compacted(before, after)
	return &compacted
}

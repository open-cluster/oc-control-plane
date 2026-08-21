package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// AN INVESTIGATION THAT PREDATES CONVERSATIONS.
//
// Every other test here writes rows through today's code, which is exactly why none of
// them can catch this: they all write the new shape. A deployment that has been running
// for months holds investigations written before any of this existed — no conversation, no
// turn, no answer, no lease, and findings whose kinds come from the vocabulary as it was.
//
// This writes that row as the old code would have, in SQL, and then reads it back through
// today's store. What it pins is that extending a persisted vocabulary and adding columns
// beside it did not quietly change what an existing row MEANS.

func TestAnInvestigationWrittenBeforeConversationsStillReadsBack(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	ctx := context.Background()

	id := uuid.New()
	window := time.Now().UTC().Add(-2 * time.Hour)

	// The old shape. Note what is ABSENT: conversation_id, turn, answer, and every lease
	// column. An existing row has none of them, and the read path must not require them.
	// The findings carry kinds and confidences from the vocabulary as it was frozen
	// before `observation` was added, and stopped_by carries a word from before `context`
	// was added.
	if _, err = pool.Exec(ctx, `
		INSERT INTO investigation (investigation_id, org_id, subject, question,
		                           window_from, window_until, status, findings,
		                           next_steps, stopped_by, spend_input_tokens,
		                           spend_output_tokens, spend_micro_cents,
		                           created_by, created_at, concluded_at)
		VALUES ($1, $2, $3, $4, $5, $6, 2, $7::jsonb, $8, $9, 900, 90, 45, $10, $11, $12)`,
		id, organization.String(), "checkout latency", "what changed?",
		window, window.Add(time.Hour), `[
			{"statement": "commit abc123 raised the connection pool timeout",
			 "kind": "probable_cause", "confidence": "likely", "sources": [2]},
			{"statement": "the database was not saturated",
			 "kind": "ruled_out", "confidence": "confirmed", "sources": [1]}
		]`, []string{"roll the pool timeout back"}, investigation.StoppedBySpend,
		"operator@example.com", window, window.Add(time.Hour)); err != nil {
		t.Fatalf("writing an investigation in the pre-conversation shape: %v", err)
	}

	read, err := placements.Investigation(ctx, organization, id)
	if err != nil {
		t.Fatalf("reading an investigation written before conversations existed: %v", err)
	}

	if read.Subject != "checkout latency" || read.Question != "what changed?" {
		t.Errorf("subject=%q question=%q; the row's own fields must survive untouched",
			read.Subject, read.Question)
	}
	if read.Status != investigation.StatusConcluded {
		t.Errorf("status = %v, want concluded", read.Status)
	}
	// The new columns read as their zero values rather than as an error. An old
	// investigation belongs to no conversation, is nobody's turn, and answered nothing —
	// all three are true, and none of them is a failure.
	if read.ConversationID != uuid.Nil {
		t.Errorf("conversationId = %v; an old investigation belongs to no conversation",
			read.ConversationID)
	}
	if read.Turn != 0 {
		t.Errorf("turn = %d; an old investigation is nobody's turn", read.Turn)
	}
	if read.Answer != "" {
		t.Errorf("answer = %q; an old investigation was asked for findings, not prose",
			read.Answer)
	}

	// THE FROZEN VOCABULARY, ON AN OLD ROW. Adding `observation` beside these must not
	// have changed what `probable_cause` or `ruled_out` mean, and the citations must
	// still point at the runs they pointed at.
	if len(read.Findings) != 2 {
		t.Fatalf("findings = %+v, want the two that were stored", read.Findings)
	}
	if read.Findings[0].Kind != investigation.FindingProbableCause ||
		read.Findings[0].Confidence != investigation.ConfidenceLikely {
		t.Errorf("finding = %+v; a kind stored before the vocabulary was extended must "+
			"still mean what it meant", read.Findings[0])
	}
	if read.Findings[1].Kind != investigation.FindingRuledOut {
		t.Errorf("finding = %+v, want the ruled-out kind unchanged", read.Findings[1])
	}
	if len(read.Findings[0].Sources) != 1 || read.Findings[0].Sources[0] != 2 {
		t.Errorf("sources = %v; a citation is what makes a finding followable",
			read.Findings[0].Sources)
	}
	if read.StoppedBy != investigation.StoppedBySpend {
		t.Errorf("stoppedBy = %q; a ceiling recorded before `context` was added must "+
			"still read as the ceiling it was", read.StoppedBy)
	}
	if read.Spend.MicroCents != 45 {
		t.Errorf("spend = %+v, want what was stored", read.Spend)
	}

	// And it is still LISTED. A row the operator surface cannot find is a row that has
	// been lost, whatever the read path says about it.
	member, err := authz.NewPrincipal(authz.KindUser, "operator@example.com", "Operator",
		[]authz.Membership{{Organization: organization, Role: authz.Editor}})
	if err != nil {
		t.Fatalf("naming a principal: %v", err)
	}
	page, err := placements.QueryInvestigations(ctx, member, organization,
		investigation.Page{Limit: 50})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, listed := range page.Investigations {
		if listed.ID == id {
			return
		}
	}
	t.Errorf("the investigation is not in the listing; %d were returned",
		len(page.Investigations))
}

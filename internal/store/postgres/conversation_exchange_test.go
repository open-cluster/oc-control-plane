package storage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestConversationBriefIncludesCanonicalAnswerBeforeCorrection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "recent exchange")
	say(t, database, organization, opened.ID, "is production affected?")
	turn, took, err := database.OpenTurn(ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, organization, turn.InvestigationID,
		investigation.Conclusion{Summary: "production appears affected"}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	say(t, database, organization, opened.ID, "correction: that was staging")
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"is production affected?", "production appears affected", "correction: that was staging"}
	if len(brief.Recent) != len(want) {
		t.Fatalf("recent exchange = %+v, want question, canonical answer, correction", brief.Recent)
	}
	for n, text := range want {
		if brief.Recent[n].Text != text {
			t.Fatalf("exchange[%d] = %q, want %q", n, brief.Recent[n].Text, text)
		}
	}
	if brief.Recent[0].Sequence != 1 || brief.Recent[2].Sequence != 2 ||
		brief.Recent[1].InvestigationID != turn.InvestigationID || brief.Recent[1].FromPerson ||
		brief.Recent[1].CreatedAt.IsZero() {
		t.Fatalf("exchange lost source identity: %+v", brief.Recent)
	}
	limited, err := database.ConversationBrief(ctx, organization, opened.ID, 2)
	if err != nil || len(limited.Recent) != 2 || limited.Recent[0].Text != want[1] || limited.Recent[1].Text != want[2] {
		t.Fatalf("bounded exchange = %+v, err=%v", limited.Recent, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, opened.ID, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("answer was duplicated into authored Messages: %+v", detail.Messages)
	}
}

func TestRecentAnswerMarksOptionalTextTruncation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "bounded answer")
	say(t, database, organization, opened.ID, "explain")
	turn, took, err := database.OpenTurn(ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, organization, turn.InvestigationID,
		investigation.Conclusion{Summary: strings.Repeat("界", 2000)}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Recent) != 1 || len([]rune(brief.Recent[0].Text)) > investigation.BriefMessageBound ||
		!strings.HasSuffix(brief.Recent[0].Text, " [truncated]") {
		t.Fatalf("optional answer truncation was hidden: %+v", brief.Recent)
	}
}

func TestRecentExchangePreservesMessageSequenceWhenTransactionTimesDisagree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "concurrent correction")
	say(t, database, organization, opened.ID, "production is affected")
	say(t, database, organization, opened.ID, "correction: staging is affected")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE conversation_message
		SET created_at = '2026-09-06T10:00:00Z'::timestamptz - sequence * interval '1 second'
		WHERE org_id = $1 AND conversation_id = $2`, organization.String(), opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Recent) != 2 || brief.Recent[0].Sequence != 1 || brief.Recent[1].Sequence != 2 {
		t.Fatalf("transaction timestamps reversed a durable correction: %+v", brief.Recent)
	}
}

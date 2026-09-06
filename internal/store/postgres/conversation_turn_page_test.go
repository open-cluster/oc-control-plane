package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestConversationDetailBoundsTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org, otherOrg := twoOrganizationsInOneDatabase(t)
	opened := openConversation(t, database, org, "long conversation")
	for range 201 {
		say(t, database, org, opened.ID, "continue")
		turn, took, err := database.OpenTurn(ctx, org, opened.ID, turnWindowLead)
		if err != nil || !took {
			t.Fatalf("opening turn: took=%v err=%v", took, err)
		}
		if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
			conclusionSaying("answer"), "", investigation.Usage{}); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := database.ConversationDetail(ctx, org, opened.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Turns) != 50 || detail.Turns[0].Ordinal != 1 || detail.Turns[49].Ordinal != 50 {
		t.Fatalf("detail returned %d turns; want first 50 in ascending order", len(detail.Turns))
	}
	if detail.TurnsNext == "" {
		t.Fatal("bounded detail omitted its continuation")
	}
	page, err := database.ConversationTurns(ctx, org, opened.ID, 0, detail.TurnsNext)
	if err != nil || len(page.Turns) != 50 || page.Turns[0].Ordinal != 51 {
		t.Fatalf("detail continuation = %+v err=%v", page, err)
	}
	page, err = database.ConversationTurns(ctx, org, opened.ID, 999, "")
	if err != nil || len(page.Turns) != 200 || page.Next == "" {
		t.Fatalf("maximum page returned %d turns, next=%q err=%v", len(page.Turns), page.Next, err)
	}
	page, err = database.ConversationTurns(ctx, org, opened.ID, 200, page.Next)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].Ordinal != 201 || page.Next != "" {
		t.Fatalf("last page = %+v err=%v", page, err)
	}
	other := openConversation(t, database, otherOrg, "another organization")
	if _, err = database.ConversationTurns(ctx, otherOrg, other.ID, 50, detail.TurnsNext); !errors.Is(err, conversation.ErrBadCursor) {
		t.Fatalf("cursor reused across Organizations: %v", err)
	}
	for _, id := range []uuid.UUID{opened.ID, uuid.New()} {
		if _, err = database.ConversationTurns(ctx, otherOrg, id, 50, ""); !errors.Is(err, conversation.ErrUnknown) {
			t.Fatalf("foreign or missing Conversation: %v", err)
		}
	}
}

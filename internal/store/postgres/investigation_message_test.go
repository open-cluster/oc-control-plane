package storage_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestInvestigationMessagesPreserveOnlyTheirAssignedBatch(t *testing.T) {
	database, org, other := twoOrganizationsInOneDatabase(t)
	chat := openConversation(t, database, org, "assigned input")
	ctx := context.Background()
	var want []investigation.AssignedMessage
	for n := range 15 {
		message := say(t, database, org, chat.ID, strings.Repeat("界", 1100)+fmt.Sprintf(" correction-%d", n))
		want = append(want, investigation.AssignedMessage{Sequence: message.Sequence, Actor: message.ActorDisplay,
			CreatedAt: message.CreatedAt, Text: message.Text})
	}
	turn, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead)
	if err != nil || !started {
		t.Fatalf("opening turn: %v %v", started, err)
	}
	say(t, database, org, chat.ID, "later queued request must not become current input")
	got, err := database.InvestigationMessages(ctx, org, chat.ID, turn.InvestigationID)
	if err != nil || len(got) != len(want) {
		t.Fatalf("assigned Messages: count=%d err=%v", len(got), err)
	}
	for n := range want {
		if got[n].Sequence != want[n].Sequence || got[n].Actor != want[n].Actor ||
			!got[n].CreatedAt.Equal(want[n].CreatedAt) || got[n].Text != want[n].Text {
			t.Fatalf("assigned Message %d lost ordering, attribution, time or text", n)
		}
	}
	if _, err := database.InvestigationMessages(ctx, other, chat.ID, turn.InvestigationID); err == nil {
		t.Fatal("another Organization read assigned input")
	}
	sibling := openConversation(t, database, org, "other Conversation")
	if _, err := database.InvestigationMessages(ctx, org, sibling.ID, turn.InvestigationID); err == nil {
		t.Fatal("another Conversation read assigned input")
	}
}

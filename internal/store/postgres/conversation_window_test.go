package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

func TestQueuedMessageWindowSurvivesDrain(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			chat := openConversation(t, database, org, "queued windows")
			appendMessage := func(window *conversation.Window) (conversation.Message, conversation.Turn, bool, error) {
				return database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, chat.ID,
					conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal, ActorID: "user-under-test", Text: "question", Window: window}, time.Hour, 100)
			}
			_, first, opened, err := appendMessage(nil)
			if err != nil || !opened {
				t.Fatalf("first: %v %v", opened, err)
			}
			var requested *conversation.Window
			if explicit {
				requested = &conversation.Window{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
			}
			queued, _, opened, err := appendMessage(requested)
			if err != nil || opened || queued.WindowFrom.IsZero() || queued.WindowUntil.IsZero() {
				t.Fatalf("queued window: %+v opened=%v err=%v", queued, opened, err)
			}
			inherited, _, _, err := appendMessage(nil)
			if err != nil || !inherited.WindowFrom.Equal(queued.WindowFrom) || !inherited.WindowUntil.Equal(queued.WindowUntil) {
				t.Fatalf("window inheritance: %+v err=%v", inherited, err)
			}
			_, _, _, err = appendMessage(&conversation.Window{From: queued.WindowFrom.Add(-time.Hour), Until: queued.WindowUntil})
			if !errors.Is(err, conversation.ErrWindowConflict) {
				t.Fatalf("incompatible window accepted: %v", err)
			}
			if _, err = database.CancelInvestigation(ctx, ownerOf(t, org), org, first.InvestigationID); err != nil {
				t.Fatal(err)
			}
			second, opened, err := database.OpenTurn(ctx, org, chat.ID, 365*24*time.Hour)
			if err != nil || !opened {
				t.Fatalf("drain: %v %v", opened, err)
			}
			found, err := database.Investigation(ctx, org, second.InvestigationID)
			if err != nil || !found.WindowFrom.Equal(queued.WindowFrom) || !found.WindowUntil.Equal(queued.WindowUntil) {
				t.Fatalf("drain shifted window: %+v err=%v", found, err)
			}
			detail, err := database.ConversationDetail(ctx, org, chat.ID, 10)
			if err != nil || len(detail.Messages) != 3 {
				t.Fatalf("refusal wrote a Message: %d %v", len(detail.Messages), err)
			}
		})
	}
}

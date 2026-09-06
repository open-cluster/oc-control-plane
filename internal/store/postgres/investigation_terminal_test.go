package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestConclusionCommitsItsReplayEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	conversation := openConversation(t, database, org, "durable completion")
	say(t, database, org, conversation.ID, "what happened?")
	turn, took, err := database.OpenTurn(ctx, org, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
		claimToken(t, database, org, turn.InvestigationID), conclusionSaying("canonical answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	events, err := database.Events(ctx, org, turn.InvestigationID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != investigation.EventConcluded ||
		events[0].Payload["summary"] != "canonical answer" {
		t.Fatalf("completion returned without its replay event: %+v", events)
	}
}

func TestTerminalEventFailureRollsBackOutcome(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "concluded", true: "failed"}[failure], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			conversation := openConversation(t, database, org, "terminal rollback")
			say(t, database, org, conversation.ID, "what happened?")
			turn, took, err := database.OpenTurn(ctx, org, conversation.ID, turnWindowLead)
			if err != nil || !took {
				t.Fatalf("opening turn: took=%v err=%v", took, err)
			}
			pool, err := database.Pool(org)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event ADD CONSTRAINT reject_terminal_test CHECK (type NOT IN (6, 7))`); err != nil {
				t.Fatal(err)
			}
			finish := func() error {
				if failure {
					return database.FailInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), "provider unavailable", investigation.Usage{InputTokens: 42})
				}
				return database.ConcludeInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), conclusionSaying("answer"), "", investigation.Usage{InputTokens: 42})
			}
			if err = finish(); err == nil {
				t.Fatal("terminal event failure was swallowed")
			}
			found, err := database.Investigation(ctx, org, turn.InvestigationID)
			if err != nil {
				t.Fatal(err)
			}
			if found.Status != investigation.StatusRunning || found.Usage.InputTokens != 0 || found.Conclusion.Summary != "" || !found.ConcludedAt.IsZero() {
				t.Fatalf("terminal failure committed outcome: %+v", found)
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event DROP CONSTRAINT reject_terminal_test`); err != nil {
				t.Fatal(err)
			}
			if err = finish(); err != nil {
				t.Fatal(err)
			}
			events, err := database.Events(ctx, org, turn.InvestigationID, 0, 0)
			if err != nil || len(events) != 1 || events[0].Sequence != 1 || !events[0].Type.Terminal() {
				t.Fatalf("retry lost terminal event or sequence: %+v err=%v", events, err)
			}
		})
	}
}

func TestLateProgressCannotFollowCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	conversation := openConversation(t, database, org, "no late progress")
	say(t, database, org, conversation.ID, "what happened?")
	turn, took, err := database.OpenTurn(ctx, org, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), conclusionSaying("done"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err = database.AppendEvent(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), investigation.Event{Sequence: 2, Type: investigation.EventProgress}); err == nil {
		t.Fatal("completed Investigation accepted late activity")
	}
}

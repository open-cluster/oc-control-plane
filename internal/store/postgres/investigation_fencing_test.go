package storage_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestExpiredWorkerCannotConclude(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	if err := database.ConcludeInvestigation(ctx, org, id, claimToken(t, database, org, id), conclusionSaying("late answer"), "", investigation.Usage{}); err == nil {
		t.Fatal("expired worker committed a conclusion")
	}
	found, err := database.Investigation(ctx, org, id)
	if err != nil || found.Status != investigation.StatusRunning {
		t.Fatalf("rejected conclusion changed state: status=%v err=%v", found.Status, err)
	}
}

func TestEveryWorkerWriteRequiresItsOwnClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	aTurn(t, database, org)
	aTurn(t, database, org)
	_, first, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker"))
	if err != nil || !took {
		t.Fatalf("first claim: %v %v", took, err)
	}
	_, second, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker"))
	if err != nil || !took || first.ClaimToken == uuid.Nil || first.ClaimToken == second.ClaimToken {
		t.Fatalf("claims are not unique: %v %v", took, err)
	}
	now := time.Now().UTC()
	run := investigation.ToolRun{Ordinal: 1, Tool: "read", Purpose: "inspect", Outcome: investigation.RunSucceeded,
		WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now, FinishedAt: now}
	writes := map[string]func(uuid.UUID) error{
		"conclusion": func(token uuid.UUID) error {
			return database.ConcludeInvestigation(ctx, org, first.ID, token, conclusionSaying("answer"), "", investigation.Usage{})
		},
		"failure": func(token uuid.UUID) error {
			return database.FailInvestigation(ctx, org, first.ID, token, "failure", investigation.Usage{})
		},
		"tool run": func(token uuid.UUID) error { return database.RecordToolRun(ctx, org, first.ID, token, run) },
		"progress": func(token uuid.UUID) error {
			return database.AppendEvent(ctx, org, first.ID, token, investigation.Event{At: now, Type: investigation.EventProgress})
		},
	}
	for _, token := range []uuid.UUID{uuid.Nil, uuid.New(), second.ClaimToken} {
		for name, write := range writes {
			if err := write(token); err == nil {
				t.Errorf("%s accepted another claim", name)
			}
		}
		if held, err := database.Heartbeat(ctx, org, first.ID, investigation.Claim{Worker: "same-worker", LeaseFor: turnWindowLead, Token: token}); err != nil || held {
			t.Errorf("wrong claim heartbeat: %v %v", held, err)
		}
	}
	if events, err := database.Events(ctx, org, first.ID, 0, 0); err != nil || len(events) != 0 {
		t.Fatalf("rejected writes left events: %v %v", events, err)
	}
	for _, name := range []string{"progress", "tool run", "conclusion"} {
		if err := writes[name](first.ClaimToken); err != nil {
			t.Fatalf("valid %s: %v", name, err)
		}
	}
	for name, write := range writes {
		if err := write(first.ClaimToken); err == nil {
			t.Errorf("terminal claim accepted %s", name)
		}
	}
	events, err := database.Events(ctx, org, first.ID, 0, 0)
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || !events[1].Type.Terminal() {
		t.Fatalf("canonical ending: %+v %v", events, err)
	}
}

func TestConcurrentClaimWritesReceiveDurableSequences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	_, opened, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker"))
	if err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := database.AppendEvent(ctx, org, id, opened.ClaimToken, investigation.Event{Sequence: 1, At: time.Now(), Type: investigation.EventProgress}); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	workers.Wait()
	events, err := database.Events(ctx, org, id, 0, 0)
	if err != nil || len(events) != 10 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("position %d has sequence %d", index, event.Sequence)
		}
	}
}

func TestExpiredWorkerCannotRecordToolRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	now := time.Now().UTC()
	run := investigation.ToolRun{Ordinal: 1, Tool: "read", Purpose: "inspect", Outcome: investigation.RunSucceeded,
		WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now, FinishedAt: now}
	if err := database.RecordToolRun(ctx, org, id, claimToken(t, database, org, id), run); err == nil {
		t.Fatal("expired worker persisted a Tool Run")
	}
}

func TestExpiredWorkerCannotAppendProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	if err := database.AppendEvent(ctx, org, id, claimToken(t, database, org, id), investigation.Event{Sequence: 1, At: time.Now(), Type: investigation.EventProgress}); err == nil {
		t.Fatal("expired worker appended progress")
	}
}

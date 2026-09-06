package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestCancellationRacesCompletionAndStaleOwnership(t *testing.T) {
	database, org := migratedDatabase(t)
	ctx := context.Background()
	for range 10 {
		id := aTurn(t, database, org)
		token := claimToken(t, database, org, id)
		start := make(chan struct{})
		finished := make(chan error, 2)
		stale := make(chan error, 1)
		go func() {
			<-start
			finished <- database.ConcludeInvestigation(ctx, org, id, token,
				conclusionSaying("committed answer"), "", investigation.Usage{})
		}()
		principal := ownerOf(t, org)
		go func() {
			<-start
			_, err := database.CancelInvestigation(ctx, principal, org, id)
			finished <- err
		}()
		go func() {
			<-start
			stale <- database.FailInvestigation(ctx, org, id, uuid.New(), "stale failure", investigation.Usage{})
		}()
		close(start)
		winners := 0
		for range 2 {
			err := <-finished
			if err == nil {
				winners++
			} else if !errors.Is(err, investigation.ErrUnknown) && !errors.Is(err, investigation.ErrAlreadyEnded) {
				t.Fatal(err)
			}
		}
		if err := <-stale; !errors.Is(err, investigation.ErrUnknown) {
			t.Fatalf("stale writer result = %v", err)
		}
		if winners != 1 {
			t.Fatalf("terminal winners=%d, want one", winners)
		}
		events, err := database.Events(ctx, org, id, 0, 0)
		if err != nil || len(events) != 1 {
			t.Fatalf("events=%+v, error=%v", events, err)
		}
		result, err := database.Investigation(ctx, org, id)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == investigation.StatusConcluded {
			if events[0].Type != investigation.EventConcluded || events[0].Payload["summary"] != result.Conclusion.Summary {
				t.Fatal("conclusion and replay disagree")
			}
		} else if result.Status != investigation.StatusCancelled || events[0].Type != investigation.EventCancelled {
			t.Fatal("cancellation and replay disagree")
		}
	}
}

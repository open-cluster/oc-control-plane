package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestBatchAdmissionSerializesWithManualAndConversationProducers(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	principal := ownerOf(t, organization)
	chat := openConversation(t, database, organization, "service")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := storage.ReserveWaitingInvestigationsForTest(ctx, tx, organization, 2, 2); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tx.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, subject, window_from, window_until)
			VALUES ($1, $2, 'batch', now(), now())`, uuid.New(), organization); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	go func() {
		_, err := database.CreateInvestigation(ctx, principal, organization,
			investigation.NewInvestigation{Subject: "manual", WindowFrom: time.Now(), WindowUntil: time.Now()}, 2)
		results <- err
	}()
	go func() {
		_, _, _, err := database.AppendMessageAndOpenTurn(ctx, principal, organization, chat.ID,
			conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
				ActorID: principal.UserID().String(), Text: "investigate"}, time.Hour, 2)
		results <- err
	}()
	awaitInvestigationAdmissionWaiters(t, ctx, pool, 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-results:
			if !errors.Is(err, investigation.ErrQueueFull) && !errors.Is(err, conversation.ErrQueueFull) {
				t.Fatalf("single-slot producer after batch commit: %v, want queue full", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var waiting int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM investigation
		WHERE org_id = $1 AND status = 1 AND lease_worker = ''`, organization).Scan(&waiting); err != nil || waiting != 2 {
		t.Fatalf("waiting=%d err=%v, want two", waiting, err)
	}
}

func awaitInvestigationAdmissionWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wanted int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event = 'advisory'
			AND query LIKE '%pg_advisory_xact_lock(hashtextextended%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == wanted {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("admission waiters=%d, want %d: %v", waiting, wanted, ctx.Err())
		}
	}
}

func TestBatchAdmissionCapacityBoundaries(t *testing.T) {
	database, organization, other := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	if _, err := database.CreateInvestigation(ctx, ownerOf(t, organization), organization,
		investigation.NewInvestigation{Subject: "service", WindowFrom: time.Now(), WindowUntil: time.Now()}, 0); err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		org       uuid.UUID
		maximum   int
		requested int
		full      bool
	}{
		{name: "exact fit", org: organization, maximum: 3, requested: 2},
		{name: "over capacity", org: organization, maximum: 2, requested: 2, full: true},
		{name: "batch larger than limit", org: other, maximum: 2, requested: 3, full: true},
		{name: "other Organization", org: other, maximum: 2, requested: 2},
		{name: "unlimited zero", org: organization, maximum: 0, requested: 100},
		{name: "unlimited negative", org: organization, maximum: -1, requested: 100},
		{name: "zero requested", org: organization, maximum: 1, requested: 0},
		{name: "largest request", org: organization, maximum: int(^uint(0) >> 1), requested: int(^uint(0) >> 1), full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			err = storage.ReserveWaitingInvestigationsForTest(ctx, tx, test.org, test.maximum, test.requested)
			if test.full && !errors.Is(err, storage.ErrWebhookJobCapacity) || !test.full && err != nil {
				t.Fatalf("capacity error = %v, want full=%t", err, test.full)
			}
		})
	}
}

package storage_test

import (
	"context"
	"testing"
	"time"

	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestRelayConflictStateRollsBackWhenItsAuditEventFails(t *testing.T) {
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event ADD CONSTRAINT reject_detection
		CHECK (action <> 'relay.conflict.detected')`); err != nil {
		t.Fatal(err)
	}
	if err = database.RecordSessionConflict(ctx, organization, registration, 2); err == nil {
		t.Fatal("conflict state committed without its Audit Event")
	}
	if conflict, readErr := database.SessionConflict(ctx, organization, registration); readErr != nil || !conflict.DetectedAt.IsZero() {
		t.Fatalf("failed detection changed current state: %+v %v", conflict, readErr)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event DROP CONSTRAINT reject_detection`); err != nil {
		t.Fatal(err)
	}
	if err = database.RecordSessionConflict(ctx, organization, registration, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event ADD CONSTRAINT reject_withdrawal
		CHECK (action <> 'relay.conflict.cleared')`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ClearSessionConflict(ctx, ownerOf(t, organization), organization, registration); err == nil {
		t.Fatal("conflict withdrawal committed without its Audit Event")
	}
	conflict, err := database.SessionConflict(ctx, organization, registration)
	if err != nil || conflict.DetectedAt.IsZero() || conflict.DistinctHosts != 2 {
		t.Fatalf("failed withdrawal changed current state: %+v %v", conflict, err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event DROP CONSTRAINT reject_withdrawal`); err != nil {
		t.Fatal(err)
	}
	trail, err := database.SessionConflictTrail(ctx, ownerOf(t, organization), organization,
		registration, storage.Page{Limit: 10})
	if err != nil || len(trail.Events) != 1 {
		t.Fatalf("conflict trail before retention = %+v %v", trail, err)
	}
	if _, err = database.PruneEventsBefore(ctx, organization, time.Now().Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	trail, err = database.SessionConflictTrail(ctx, ownerOf(t, organization), organization,
		registration, storage.Page{Limit: 10})
	if err != nil || len(trail.Events) != 0 {
		t.Fatalf("conflict trail survived Audit Event retention: %+v %v", trail, err)
	}
}

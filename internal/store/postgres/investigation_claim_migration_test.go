package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestClaimMigrationPreservesLeasedAndQueuedInvestigations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	first := aTurn(t, database, org)
	second := aTurn(t, database, org)
	queued := aTurn(t, database, org)
	for range 2 {
		if _, _, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker")); err != nil || !took {
			t.Fatalf("claim: %v %v", took, err)
		}
	}
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE investigation DROP COLUMN lease_token; DELETE FROM schema_migration WHERE version = '0006_investigation_claim_token'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	firstToken := claimToken(t, database, org, first)
	secondToken := claimToken(t, database, org, second)
	if firstToken == uuid.Nil || secondToken == uuid.Nil || firstToken == secondToken {
		t.Fatal("retained claims lack distinct tokens")
	}
	var queuedToken *uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT lease_token FROM investigation WHERE org_id = $1 AND investigation_id = $2`, org.String(), queued).Scan(&queuedToken); err != nil || queuedToken != nil {
		t.Fatalf("queued ownership changed: %v %v", queuedToken, err)
	}
	expireInvestigationLease(t, database, org, first)
	if count, err := database.RecoverStale(ctx, investigation.RecoveryReason, 10); err != nil || count != 1 {
		t.Fatalf("recovery after migration: %d %v", count, err)
	}
	if err = database.ConcludeInvestigation(ctx, org, second, secondToken, conclusionSaying("retained answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	_, claimed, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker"))
	if err != nil || !took || claimed.ID != queued || claimed.ClaimToken == uuid.Nil || claimed.ClaimToken == secondToken {
		t.Fatalf("queued claim after migration: %+v took=%v err=%v", claimed, took, err)
	}
}

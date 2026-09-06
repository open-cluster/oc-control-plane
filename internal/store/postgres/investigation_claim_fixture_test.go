package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func claimToken(t *testing.T, database *storage.Database, org tenancy.Organization, id uuid.UUID) uuid.UUID {
	t.Helper()
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = pool.Exec(ctx, `UPDATE investigation
		SET lease_worker = 'fixture', lease_token = gen_random_uuid(), lease_expires_at = now() + interval '15 minutes'
		WHERE org_id = $1 AND investigation_id = $2 AND status = 1 AND lease_worker = ''`, org.String(), id); err != nil {
		t.Fatal(err)
	}
	var value *uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT lease_token FROM investigation WHERE org_id = $1 AND investigation_id = $2`, org.String(), id).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil
		}
		t.Fatal(err)
	}
	if value == nil {
		return uuid.Nil
	}
	return *value
}

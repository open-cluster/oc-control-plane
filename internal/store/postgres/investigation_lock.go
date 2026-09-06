package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Lock before checking ownership so time spent waiting cannot revive an expired claim.
func lockInvestigation(ctx context.Context, tx pgx.Tx, org tenancy.Organization, id uuid.UUID) error {
	var found int
	err := tx.QueryRow(ctx, `SELECT 1 FROM investigation WHERE org_id = $1 AND investigation_id = $2 FOR NO KEY UPDATE`, org.String(), id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.ErrUnknown
	}
	return err
}

package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrInvestigationCapacity = errors.New("organization has reached its waiting investigation limit")

func reserveWaitingInvestigations(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID, maximum, requested int,
) error {
	if maximum <= 0 {
		return nil
	}
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		organization.String()); err != nil {
		return fmt.Errorf("locking organization waiting-investigation capacity: %w", err)
	}
	var waiting int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM investigation
		 WHERE org_id = $1 AND status = 1 AND lease_worker = ''`,
		organization).Scan(&waiting); err != nil {
		return fmt.Errorf("counting organization waiting investigations: %w", err)
	}
	if requested > maximum-waiting {
		return ErrInvestigationCapacity
	}
	return nil
}

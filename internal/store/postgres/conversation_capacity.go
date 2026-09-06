package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

const maxQueuedMessages = 100

func reserveQueuedMessage(ctx context.Context, tx pgx.Tx, organization tenancy.Organization) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, organization.String()); err != nil {
		return fmt.Errorf("locking organization Message capacity: %w", err)
	}
	var queued int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversation_message
		WHERE org_id = $1 AND investigation_id IS NULL AND role = 1`, organization.String()).Scan(&queued); err != nil {
		return fmt.Errorf("counting queued Messages: %w", err)
	}
	if queued >= maxQueuedMessages {
		return conversation.ErrQueueFull
	}
	return nil
}

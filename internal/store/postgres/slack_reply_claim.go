package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

// ReleaseSlackReply schedules a normal flush independently of crash recovery.
func (p *Database) ReleaseSlackReply(ctx context.Context, org tenancy.Organization, id, owner uuid.UUID, at time.Time) error {
	return p.withSlackReplyClaim(ctx, org, id, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE slack_reply SET status = $3, lease_owner = NULL,
			leased_until = NULL, next_attempt_at = $4, updated_at = now()
			WHERE org_id = $1 AND investigation_id = $2`, org.String(), id, SlackReplyPending, at.UTC())
		return err
	})
}

func (p *Database) withSlackReplyClaim(
	ctx context.Context, org tenancy.Organization, id, owner uuid.UUID,
	write func(pgx.Tx) error,
) error {
	pool, err := p.Pool(org)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT investigation_id FROM slack_reply
		WHERE org_id = $1 AND investigation_id = $2 FOR UPDATE`, org.String(), id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return slack.ErrReplyClaimLost
	}
	if err != nil {
		return fmt.Errorf("locking a slack reply: %w", err)
	}
	// Check expiry after acquiring the lock; waiting for it may outlive the claim.
	var owned bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE(status = $4 AND lease_owner = $3
		AND leased_until > clock_timestamp(), false) FROM slack_reply
		WHERE org_id = $1 AND investigation_id = $2`, org.String(), id, owner, SlackReplyDelivering).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return slack.ErrReplyClaimLost
	}
	if err = write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

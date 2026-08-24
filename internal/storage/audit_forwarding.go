package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ClaimAuditDeliveries leases ready events across Organizations. This is a deployment worker
// discovery query, not an Organization-owned read; each returned event retains its org_id.
func (d *Database) ClaimAuditDeliveries(
	ctx context.Context, owner string, now, leaseUntil time.Time, limit int,
) ([]audit.ForwardingDelivery, error) {
	if owner == "" || limit <= 0 || !leaseUntil.After(now) {
		return nil, errors.New("claiming audit deliveries requires an owner and positive bounds")
	}
	rows, err := d.pool.Query(ctx, `
		WITH ready AS (
			SELECT event_id
			  FROM audit_forwarding_outbox
			 WHERE terminal = FALSE
			   AND next_attempt_at <= $2
			   AND (lease_until IS NULL OR lease_until <= $2)
			 ORDER BY next_attempt_at, event_id
			 FOR UPDATE SKIP LOCKED
			 LIMIT $4
		)
		UPDATE audit_forwarding_outbox queued
		   SET lease_owner = $1, lease_until = $3, last_attempt_at = $2
		  FROM ready
		 WHERE queued.event_id = ready.event_id
		RETURNING queued.event_payload, queued.attempts`, owner, now, leaseUntil, limit)
	if err != nil {
		return nil, fmt.Errorf("claiming audit deliveries: %w", err)
	}
	defer rows.Close()
	deliveries := make([]audit.ForwardingDelivery, 0, limit)
	for rows.Next() {
		var payload []byte
		var delivery audit.ForwardingDelivery
		if err = rows.Scan(&payload, &delivery.Attempts); err != nil {
			return nil, fmt.Errorf("scanning an audit delivery: %w", err)
		}
		if err = json.Unmarshal(payload, &delivery.Event); err != nil {
			return nil, fmt.Errorf("decoding an audit delivery: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming audit deliveries: %w", err)
	}
	return deliveries, nil
}

// CompleteAuditDelivery removes a row only for the worker holding its live lease.
func (d *Database) CompleteAuditDelivery(ctx context.Context, owner, eventID string) error {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM audit_forwarding_outbox
		 WHERE event_id = $1 AND lease_owner = $2`, eventID, owner)
	if err != nil {
		return fmt.Errorf("completing an audit delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("completing an audit delivery: lease is not held")
	}
	return nil
}

// FailAuditDelivery records only a safe classification. A remote error may contain echoed
// request data or credentials and must never enter PostgreSQL.
func (d *Database) FailAuditDelivery(
	ctx context.Context, owner, eventID string, next time.Time, terminal bool,
) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE audit_forwarding_outbox
		   SET attempts = attempts + 1,
		       terminal = $3,
		       next_attempt_at = $4,
		       lease_owner = NULL,
		       lease_until = NULL,
		       last_error = 'remote delivery failed'
		 WHERE event_id = $1 AND lease_owner = $2`, eventID, owner, terminal, next)
	if err != nil {
		return fmt.Errorf("recording an audit delivery failure: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("recording an audit delivery failure: lease is not held")
	}
	return nil
}

// ReplayAuditDelivery returns a terminal row to the ready queue without changing its event.
func (d *Database) ReplayAuditDelivery(
	ctx context.Context, organization tenancy.Organization, eventID uuid.UUID,
) error {
	if organization.IsEmpty() {
		return ErrUnknownOrganization
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE audit_forwarding_outbox
		   SET attempts = 0, terminal = FALSE, next_attempt_at = now(),
		       lease_owner = NULL, lease_until = NULL, last_error = ''
		 WHERE org_id = $1 AND event_id = $2 AND terminal = TRUE`, organization.String(), eventID)
	if err != nil {
		return fmt.Errorf("replaying an audit delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

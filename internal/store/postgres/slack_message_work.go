package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SlackMessageWorkStatus is the persisted lifecycle state of one accepted Slack Message.
type SlackMessageWorkStatus int16

// MaxSlackMessageAttempts is frozen by the persisted job-row CHECK constraint.
const MaxSlackMessageAttempts = 12

const (
	SlackMessageReady SlackMessageWorkStatus = iota + 1
	SlackMessageLeased
	SlackMessageRetry
	SlackMessageTerminal
	SlackMessageComplete
)

// String returns the stable operator-facing name of a Slack Message work status.
func (status SlackMessageWorkStatus) String() string {
	names := [...]string{"unknown", "ready", "leased", "retry", "terminal", "complete"}
	if status <= 0 || int(status) >= len(names) {
		return names[0]
	}
	return names[status]
}

// ErrSlackMessageLeaseLost means a transition no longer owns the fenced Slack Message lease.
var ErrSlackMessageLeaseLost = errors.New("slack message work lease is no longer held")

// SlackMessageWork is one durable, fenced Slack Message processing attempt.
type SlackMessageWork struct {
	ID              uuid.UUID
	Organization    uuid.UUID
	Status          SlackMessageWorkStatus
	DeliveryID      uuid.UUID
	IntegrationID   uuid.UUID
	ConversationID  uuid.UUID
	MessageSequence int64
	Attempts        int
	LeaseOwner      string
	LeaseEpoch      int64
	FailureClass    string
	FailureMessage  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ApplySlackMessageWork opens the next Conversation turn through the existing queue seam
// and advances the fenced Slack Message work atomically. The Message assignment is the durable
// idempotency boundary when a prior attempt already opened the turn.
func (d *Database) ApplySlackMessageWork(
	ctx context.Context, organization uuid.UUID, work SlackMessageWork,
	windowLead time.Duration, maxWaiting int,
) error {
	work.Organization = organization
	pool, err := d.Pool(organization)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning Slack delivery processing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = reserveWaitingInvestigations(ctx, tx, work.Organization, maxWaiting, 1); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT webhook_turn`); err != nil {
		return fmt.Errorf("setting a slack turn savepoint: %w", err)
	}
	_, opened, err := openTurn(ctx, tx, work.Organization, work.ConversationID, windowLead)
	if err != nil {
		return err
	}
	if !opened {
		if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT webhook_turn`); err != nil {
			return fmt.Errorf("preserving a queued slack message: %w", err)
		}
	}
	if err = completeSlackMessageWorkTx(ctx, tx, work); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing Slack delivery processing: %w", err)
	}
	return nil
}

func completeSlackMessageWorkTx(ctx context.Context, tx pgx.Tx, work SlackMessageWork) error {
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_job
		   SET status = 5, lease_owner = '', lease_expires_at = NULL, updated_at = now()
		 WHERE org_id = $1 AND job_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization, work.ID, work.LeaseOwner, work.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("completing webhook delivery effect: %w", err)
	}
	return requireSlackMessageLease(tag.RowsAffected())
}

func enqueueSlackMessageWork(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	deliveryID, integrationID, conversationID uuid.UUID,
	messageSequence int64,
) error {
	if _, err := transaction.Exec(ctx, `
		INSERT INTO webhook_job
			(job_id, org_id, delivery_id, integration_id,
			 conversation_id, message_sequence, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT DO NOTHING`, uuid.New(), organization, deliveryID,
		integrationID, conversationID, messageSequence); err != nil {
		return fmt.Errorf("enqueueing slack message work: %w", err)
	}
	return nil
}

// ClaimSlackMessageWork discovers ready work across Organizations. The returned Organization is
// authoritative for every later transition, which must also present the lease epoch.
func (d *Database) ClaimSlackMessageWork(
	ctx context.Context, owner string, lease time.Duration,
) (SlackMessageWork, bool, error) {
	if owner == "" || lease <= 0 {
		return SlackMessageWork{}, false, errors.New("slack message work owner and lease are required")
	}
	var work SlackMessageWork
	var conversationID *uuid.UUID
	err := d.pool.QueryRow(ctx, `
			WITH selected AS (
				SELECT org_id, job_id
				  FROM webhook_job
				 WHERE (status IN (1, 3) AND available_at <= now())
				    OR (status = 2 AND lease_expires_at <= now())
				 ORDER BY available_at, created_at, job_id
				 FOR UPDATE SKIP LOCKED
				 LIMIT 1
			)
			UPDATE webhook_job AS work
			   SET status = 2, lease_owner = $1, lease_epoch = work.lease_epoch + 1,
			       lease_expires_at = now() + $2::interval,
			       attempts = CASE WHEN work.attempts >= $3 THEN work.attempts
			                       ELSE work.attempts + 1 END,
			       failure_class = '', failure_message = '',
			       updated_at = now()
			  FROM selected
			 WHERE work.org_id = selected.org_id AND work.job_id = selected.job_id
			RETURNING work.job_id, work.org_id, work.status, work.delivery_id,
			          work.integration_id, work.conversation_id,
			          coalesce(work.message_sequence, 0), work.attempts, work.lease_owner,
			          work.lease_epoch, work.created_at, work.updated_at`,
		owner, lease.String(), MaxSlackMessageAttempts).Scan(&work.ID, &work.Organization, &work.Status,
		&work.DeliveryID, &work.IntegrationID, &conversationID,
		&work.MessageSequence, &work.Attempts, &work.LeaseOwner, &work.LeaseEpoch,
		&work.CreatedAt, &work.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlackMessageWork{}, false, nil
	}
	if err != nil {
		return SlackMessageWork{}, false, fmt.Errorf("claiming slack message work: %w", err)
	}
	if conversationID != nil {
		work.ConversationID = *conversationID
	}
	return work, true, nil
}

// HeartbeatSlackMessageWork renews a currently fenced Slack Message lease.
func (d *Database) HeartbeatSlackMessageWork(
	ctx context.Context, organization uuid.UUID, work SlackMessageWork, lease time.Duration,
) error {
	work.Organization = organization
	pool, err := d.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_job
		   SET lease_expires_at = now() + $5::interval, updated_at = now()
		 WHERE org_id = $1 AND job_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization, work.ID, work.LeaseOwner, work.LeaseEpoch, lease.String())
	if err != nil {
		return fmt.Errorf("renewing slack message work lease: %w", err)
	}
	return requireSlackMessageLease(tag.RowsAffected())
}

// FailSlackMessageWork records a retryable or terminal Slack Message processing failure.
func (d *Database) FailSlackMessageWork(
	ctx context.Context, organization uuid.UUID, work SlackMessageWork, terminal bool, delay time.Duration,
	class, message string,
) error {
	work.Organization = organization
	status := SlackMessageRetry
	if terminal {
		status = SlackMessageTerminal
	}
	return d.transitionSlackMessageWork(ctx, work, status, delay,
		boundedText(class, 64), boundedText(message, 512))
}

// DeferSlackMessageWork preserves an accepted Message behind Organization backpressure without
// consuming its failure budget or making a permanently delayed Message terminal.
func (d *Database) DeferSlackMessageWork(
	ctx context.Context, organization uuid.UUID, work SlackMessageWork, delay time.Duration,
) error {
	pool, err := d.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_job
		   SET status = 3, attempts = greatest(attempts - 1, 0),
		       available_at = now() + $5::interval,
		       lease_owner = '', lease_expires_at = NULL,
		       failure_class = 'organization-at-capacity',
		       failure_message = 'the Organization has reached its waiting Investigation limit',
		       updated_at = now()
		 WHERE org_id = $1 AND job_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		organization, work.ID, work.LeaseOwner, work.LeaseEpoch, max(delay, 0).String())
	if err != nil {
		return fmt.Errorf("deferring webhook delivery behind Organization capacity: %w", err)
	}
	return requireSlackMessageLease(tag.RowsAffected())
}

func (d *Database) transitionSlackMessageWork(
	ctx context.Context, work SlackMessageWork, status SlackMessageWorkStatus, delay time.Duration,
	class, message string,
) error {
	pool, err := d.Pool(work.Organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_job
		   SET status = $5, available_at = now() + $6::interval,
		       lease_owner = '', lease_expires_at = NULL,
		       failure_class = $7, failure_message = $8, updated_at = now()
		 WHERE org_id = $1 AND job_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization, work.ID, work.LeaseOwner, work.LeaseEpoch,
		int16(status), max(delay, 0).String(), class, message)
	if err != nil {
		return fmt.Errorf("transitioning slack message work: %w", err)
	}
	return requireSlackMessageLease(tag.RowsAffected())
}

func requireSlackMessageLease(rows int64) error {
	if rows != 1 {
		return ErrSlackMessageLeaseLost
	}
	return nil
}

func boundedText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

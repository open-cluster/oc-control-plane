package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type WebhookWorkKind int16

const (
	WebhookWorkAlert WebhookWorkKind = iota + 1
	WebhookWorkSlack
)

func (kind WebhookWorkKind) String() string {
	names := [...]string{"unknown", "alert", "slack"}
	if kind <= 0 || int(kind) >= len(names) {
		return names[0]
	}
	return names[kind]
}

type WebhookWorkStatus int16

// MaxWebhookWorkAttempts is frozen by the persisted work-row CHECK constraint.
const MaxWebhookWorkAttempts = 12

const (
	WebhookWorkReady WebhookWorkStatus = iota + 1
	WebhookWorkLeased
	WebhookWorkRetry
	WebhookWorkTerminal
	WebhookWorkComplete
)

func (status WebhookWorkStatus) String() string {
	names := [...]string{"unknown", "ready", "leased", "retry", "terminal", "complete"}
	if status <= 0 || int(status) >= len(names) {
		return names[0]
	}
	return names[status]
}

var ErrWebhookWorkLeaseLost = errors.New("webhook delivery processing lease is no longer held")
var ErrWebhookWorkUnknown = errors.New("webhook delivery processing record not found")
var ErrWebhookWorkCapacity = errors.New("organization has reached its waiting investigation limit")

type WebhookWork struct {
	ID              uuid.UUID
	Organization    tenancy.Organization
	Kind            WebhookWorkKind
	Status          WebhookWorkStatus
	DeliveryID      uuid.UUID
	IntegrationID   uuid.UUID
	IncidentID      uuid.UUID
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

// ApplyAlertWebhookWork opens the one Investigation identified by this work item and
// advances the fenced lease in the same transaction. A retry observes the unique origin.
func (d *Database) ApplyAlertWebhookWork(
	ctx context.Context, organization tenancy.Organization, work WebhookWork,
	windowLead time.Duration, maxWaiting int,
) (uuid.UUID, error) {
	work.Organization = organization
	pool, err := d.Pool(organization)
	if err != nil {
		return uuid.Nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("beginning Alert Event delivery processing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var investigationID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT investigation_id
		  FROM investigation
		 WHERE org_id = $1 AND webhook_work_id = $2`,
		work.Organization.String(), work.ID).Scan(&investigationID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = reserveWaitingInvestigation(ctx, tx, work.Organization, maxWaiting); err != nil {
			return uuid.Nil, err
		}
		var title string
		var firstSeen, lastSeen time.Time
		if err = tx.QueryRow(ctx, `
			SELECT title, first_seen_at, last_seen_at
			  FROM incident
			 WHERE org_id = $1 AND incident_id = $2`, work.Organization.String(),
			work.IncidentID).Scan(&title, &firstSeen, &lastSeen); err != nil {
			return uuid.Nil, fmt.Errorf("reading delivery processing Incident: %w", err)
		}
		investigationID = uuid.New()
		if _, err = tx.Exec(ctx, `
			INSERT INTO investigation
				(investigation_id, org_id, incident_id, integration_id, subject,
				 window_from, window_until, created_by, webhook_work_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'webhook', $8)`, investigationID,
			work.Organization.String(), work.IncidentID, work.IntegrationID, title,
			firstSeen.Add(-windowLead), lastSeen, work.ID); err != nil {
			return uuid.Nil, fmt.Errorf("opening alert investigation: %w", err)
		}
	} else if err != nil {
		return uuid.Nil, fmt.Errorf("reading alert webhook effect: %w", err)
	}
	if err = completeWebhookWorkTx(ctx, tx, work); err != nil {
		return uuid.Nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("committing Alert Event delivery processing: %w", err)
	}
	return investigationID, nil
}

// ApplySlackWebhookWork opens the next Conversation turn through the existing queue seam
// and advances the fenced work item atomically. The Message assignment is the durable
// idempotency boundary when a prior attempt already opened the turn.
func (d *Database) ApplySlackWebhookWork(
	ctx context.Context, organization tenancy.Organization, work WebhookWork,
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
	if err = reserveWaitingInvestigation(ctx, tx, work.Organization, maxWaiting); err != nil {
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
	if err = completeWebhookWorkTx(ctx, tx, work); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing Slack delivery processing: %w", err)
	}
	return nil
}

func reserveWaitingInvestigation(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, maximum int,
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
		organization.String()).Scan(&waiting); err != nil {
		return fmt.Errorf("counting organization waiting investigations: %w", err)
	}
	if waiting >= maximum {
		return ErrWebhookWorkCapacity
	}
	return nil
}

func completeWebhookWorkTx(ctx context.Context, tx pgx.Tx, work WebhookWork) error {
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_work
		   SET status = 5, lease_owner = '', lease_expires_at = NULL, updated_at = now()
		 WHERE org_id = $1 AND work_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization.String(), work.ID, work.LeaseOwner, work.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("completing webhook delivery effect: %w", err)
	}
	return requireWorkLease(tag.RowsAffected())
}

func enqueueWebhookWork(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	kind WebhookWorkKind, deliveryID, integrationID, incidentID, conversationID uuid.UUID,
	messageSequence int64,
) error {
	var incident, conversation any
	if incidentID != uuid.Nil {
		incident = incidentID
	}
	if conversationID != uuid.Nil {
		conversation = conversationID
	}
	if _, err := transaction.Exec(ctx, `
		INSERT INTO webhook_work
			(work_id, org_id, kind, delivery_id, integration_id, incident_id,
			 conversation_id, message_sequence)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, 0))
		ON CONFLICT DO NOTHING`, uuid.New(), organization.String(), int16(kind), deliveryID,
		integrationID, incident, conversation, messageSequence); err != nil {
		return fmt.Errorf("enqueueing webhook delivery processing: %w", err)
	}
	return nil
}

// ClaimWebhookWork discovers ready work across Organizations. The returned Organization is
// authoritative for every later transition, which must also present the lease epoch.
func (d *Database) ClaimWebhookWork(
	ctx context.Context, owner string, lease time.Duration,
) (WebhookWork, bool, error) {
	if owner == "" || lease <= 0 {
		return WebhookWork{}, false, errors.New("webhook delivery processing owner and lease are required")
	}
	var work WebhookWork
	var organization string
	var incidentID, conversationID *uuid.UUID
	err := d.pool.QueryRow(ctx, `
			WITH selected AS (
				SELECT org_id, work_id
				  FROM webhook_work
				 WHERE (status IN (1, 3) AND available_at <= now())
				    OR (status = 2 AND lease_expires_at <= now())
				 ORDER BY available_at, created_at, work_id
				 FOR UPDATE SKIP LOCKED
				 LIMIT 1
			)
			UPDATE webhook_work AS work
			   SET status = 2, lease_owner = $1, lease_epoch = work.lease_epoch + 1,
			       lease_expires_at = now() + $2::interval,
			       attempts = CASE WHEN work.attempts >= $3 THEN work.attempts
			                       ELSE work.attempts + 1 END,
			       failure_class = '', failure_message = '',
			       updated_at = now()
			  FROM selected
			 WHERE work.org_id = selected.org_id AND work.work_id = selected.work_id
			RETURNING work.work_id, work.org_id, work.kind, work.status, work.delivery_id,
			          work.integration_id, work.incident_id, work.conversation_id,
			          coalesce(work.message_sequence, 0), work.attempts, work.lease_owner,
			          work.lease_epoch, work.created_at, work.updated_at`,
		owner, lease.String(), MaxWebhookWorkAttempts).Scan(&work.ID, &organization, &work.Kind, &work.Status,
		&work.DeliveryID, &work.IntegrationID, &incidentID, &conversationID,
		&work.MessageSequence, &work.Attempts, &work.LeaseOwner, &work.LeaseEpoch,
		&work.CreatedAt, &work.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookWork{}, false, nil
	}
	if err != nil {
		return WebhookWork{}, false, fmt.Errorf("claiming webhook delivery processing: %w", err)
	}
	work.Organization, err = tenancy.NewOrganization(organization)
	if err != nil {
		return WebhookWork{}, false, fmt.Errorf("claimed webhook delivery has invalid Organization: %w", err)
	}
	if incidentID != nil {
		work.IncidentID = *incidentID
	}
	if conversationID != nil {
		work.ConversationID = *conversationID
	}
	return work, true, nil
}

func (d *Database) HeartbeatWebhookWork(
	ctx context.Context, organization tenancy.Organization, work WebhookWork, lease time.Duration,
) error {
	work.Organization = organization
	pool, err := d.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_work
		   SET lease_expires_at = now() + $5::interval, updated_at = now()
		 WHERE org_id = $1 AND work_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization.String(), work.ID, work.LeaseOwner, work.LeaseEpoch, lease.String())
	if err != nil {
		return fmt.Errorf("renewing webhook delivery processing lease: %w", err)
	}
	return requireWorkLease(tag.RowsAffected())
}

func (d *Database) CompleteWebhookWork(ctx context.Context, organization tenancy.Organization, work WebhookWork) error {
	work.Organization = organization
	return d.transitionWebhookWork(ctx, work, WebhookWorkComplete, 0, "", "")
}

func (d *Database) FailWebhookWork(
	ctx context.Context, organization tenancy.Organization, work WebhookWork, terminal bool, delay time.Duration,
	class, message string,
) error {
	work.Organization = organization
	status := WebhookWorkRetry
	if terminal {
		status = WebhookWorkTerminal
	}
	return d.transitionWebhookWork(ctx, work, status, delay,
		boundedText(class, 64), boundedText(message, 512))
}

// DeferWebhookWork preserves an accepted Message behind Organization backpressure without
// consuming its failure budget or making a permanently delayed Message terminal.
func (d *Database) DeferWebhookWork(
	ctx context.Context, organization tenancy.Organization, work WebhookWork, delay time.Duration,
) error {
	pool, err := d.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_work
		   SET status = 3, attempts = greatest(attempts - 1, 0),
		       available_at = now() + $5::interval,
		       lease_owner = '', lease_expires_at = NULL,
		       failure_class = 'organization-at-capacity',
		       failure_message = 'the Organization has reached its waiting Investigation limit',
		       updated_at = now()
		 WHERE org_id = $1 AND work_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		organization.String(), work.ID, work.LeaseOwner, work.LeaseEpoch, max(delay, 0).String())
	if err != nil {
		return fmt.Errorf("deferring webhook delivery behind Organization capacity: %w", err)
	}
	return requireWorkLease(tag.RowsAffected())
}

func (d *Database) transitionWebhookWork(
	ctx context.Context, work WebhookWork, status WebhookWorkStatus, delay time.Duration,
	class, message string,
) error {
	pool, err := d.Pool(work.Organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE webhook_work
		   SET status = $5, available_at = now() + $6::interval,
		       lease_owner = '', lease_expires_at = NULL,
		       failure_class = $7, failure_message = $8, updated_at = now()
		 WHERE org_id = $1 AND work_id = $2 AND status = 2
		   AND lease_owner = $3 AND lease_epoch = $4 AND lease_expires_at > now()`,
		work.Organization.String(), work.ID, work.LeaseOwner, work.LeaseEpoch,
		int16(status), max(delay, 0).String(), class, message)
	if err != nil {
		return fmt.Errorf("transitioning webhook delivery processing: %w", err)
	}
	return requireWorkLease(tag.RowsAffected())
}

func requireWorkLease(rows int64) error {
	if rows != 1 {
		return ErrWebhookWorkLeaseLost
	}
	return nil
}

func boundedText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

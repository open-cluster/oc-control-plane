package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The delivery states, as the column stores them. Frozen, and exported so the enum gate can
// hold this file to them.
const (
	SlackDeliveryPending    = 1
	SlackDeliveryDelivering = 2
	SlackDeliveryDelivered  = 3
	SlackDeliveryFailed     = 4
)

// oweSlackDeliveries records an answer owed for every turn of a Slack conversation that has
// none yet.
//
// DERIVED RATHER THAN HOOKED, and that is the point. A turn is opened in two places — by the
// events endpoint when somebody speaks, and by the drain at a running turn's terminal boundary
// when they spoke while it was working — and a hook in one of them is a silent gap in the
// other: an investigation that ran and never answered in the thread. Reading the two records
// that already exist cannot have that gap, and it heals a deployment that was upgraded while
// turns were in flight.
//
// Idempotent, bounded, and run at the start of each claim pass.
func oweSlackDeliveries(ctx context.Context, pool *pgxpool.Pool, limit int) error {
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_delivery
			(investigation_id, org_id, integration_id, channel_id, thread_ts)
		SELECT i.investigation_id, i.org_id, s.integration_id, s.channel_id, s.thread_ts
		  FROM investigation i
		  JOIN slack_conversation s
		    ON s.org_id = i.org_id AND s.conversation_id = i.conversation_id
		 WHERE i.conversation_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM slack_delivery d
		                    WHERE d.investigation_id = i.investigation_id)
		 ORDER BY i.created_at
		 LIMIT $1
		ON CONFLICT (investigation_id) DO NOTHING`, limit); err != nil {
		return fmt.Errorf("recording owed slack deliveries: %w", err)
	}
	return nil
}

// ClaimSlackDeliveries leases the deliveries that are due, across every placement.
//
// The LEASE is what stops two workers writing into one visible message, which would be the one
// failure a reader in the thread could not make sense of. It takes no organization for the
// reason every other sweep does not: finding out which tenants owe an answer IS the question.
func (p *Placements) ClaimSlackDeliveries(
	ctx context.Context, limit int, lease time.Duration,
) ([]slack.Delivery, error) {
	var claimed []slack.Delivery
	for _, name := range p.names() {
		// Every turn of a Slack conversation owes an answer, whether it was opened by
		// somebody speaking or by the drain behind a running turn.
		if err := oweSlackDeliveries(ctx, p.pools[name], limit); err != nil {
			return nil, err
		}
		rows, err := p.pools[name].Query(ctx, `
			UPDATE slack_delivery
			   SET status       = $1,
			       leased_until = now() + $2::interval,
			       updated_at   = now()
			 WHERE investigation_id IN (
			       SELECT investigation_id
			         FROM slack_delivery
			        WHERE status IN ($3, $1)
			          AND next_attempt_at <= now()
			          AND (leased_until IS NULL OR leased_until < now())
			        ORDER BY next_attempt_at
			        LIMIT $4
			          FOR UPDATE SKIP LOCKED)
			RETURNING investigation_id, org_id, integration_id, channel_id, thread_ts,
			          stream_ts, streaming, last_sequence, attempts`,
			SlackDeliveryDelivering, lease.String(), SlackDeliveryPending, limit)
		if err != nil {
			return nil, fmt.Errorf("claiming slack deliveries: %w", err)
		}
		for rows.Next() {
			var (
				one          slack.Delivery
				organization string
			)
			if err := rows.Scan(&one.Investigation, &organization, &one.Integration,
				&one.Channel, &one.Thread, &one.StreamTS, &one.Streaming,
				&one.LastSequence, &one.Attempts); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning a slack delivery: %w", err)
			}
			named, err := tenancy.NewOrganization(organization)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf(
					"a slack delivery names an organization that is not a name: %w", err)
			}
			one.Organization = named
			claimed = append(claimed, one)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("claiming slack deliveries: %w", err)
		}
	}
	return claimed, nil
}

// AdvanceSlackDelivery records progress. The cursor only ever moves forward, which is the
// property that makes a retry append what was missed rather than repost what was seen.
func (p *Placements) AdvanceSlackDelivery(
	ctx context.Context, organization tenancy.Organization, investigation uuid.UUID,
	streamTS string, streaming bool, sequence int64,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		UPDATE slack_delivery
		   SET stream_ts     = CASE WHEN stream_ts = '' THEN $3 ELSE stream_ts END,
		       streaming     = CASE WHEN stream_ts = '' THEN $4 ELSE streaming END,
		       last_sequence = GREATEST(last_sequence, $5),
		       attempts      = 0,
		       note          = '',
		       updated_at    = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
		investigation, organization.String(), streamTS, streaming, sequence); err != nil {
		return fmt.Errorf("advancing a slack delivery: %w", err)
	}
	return nil
}

// CompleteSlackDelivery marks one delivered. Nothing claims it again.
func (p *Placements) CompleteSlackDelivery(
	ctx context.Context, organization tenancy.Organization, investigation uuid.UUID,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		UPDATE slack_delivery
		   SET status = $3, leased_until = NULL, updated_at = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
		investigation, organization.String(), SlackDeliveryDelivered); err != nil {
		return fmt.Errorf("completing a slack delivery: %w", err)
	}
	return nil
}

// RetrySlackDelivery schedules another attempt, or gives up.
//
// Giving up is TERMINAL for the delivery and says nothing about the investigation, which has
// its own status and its own record. That separation is the point: a Slack outage must not be
// able to make a completed investigation look failed.
func (p *Placements) RetrySlackDelivery(
	ctx context.Context, organization tenancy.Organization, investigation uuid.UUID,
	at time.Time, note string, giveUp bool,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	status := SlackDeliveryPending
	if giveUp {
		status = SlackDeliveryFailed
	}
	if _, err := pool.Exec(ctx, `
		UPDATE slack_delivery
		   SET status          = $3,
		       attempts        = attempts + 1,
		       next_attempt_at = $4,
		       note            = $5,
		       leased_until    = NULL,
		       updated_at      = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
		investigation, organization.String(), status, at.UTC(), note); err != nil {
		return fmt.Errorf("rescheduling a slack delivery: %w", err)
	}
	return nil
}

// SlackDeliveryState reports what one delivery looks like, for the tests and for support. It
// answers false where the investigation owes no Slack answer.
func (p *Placements) SlackDeliveryState(
	ctx context.Context, organization tenancy.Organization, investigation uuid.UUID,
) (status int, sequence int64, streamTS string, note string, found bool, err error) {
	pool, poolErr := p.Pool(organization)
	if poolErr != nil {
		return 0, 0, "", "", false, poolErr
	}
	scanErr := pool.QueryRow(ctx, `
		SELECT status, last_sequence, stream_ts, note
		  FROM slack_delivery
		 WHERE investigation_id = $1 AND org_id = $2`,
		investigation, organization.String()).Scan(&status, &sequence, &streamTS, &note)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return 0, 0, "", "", false, nil
	case scanErr != nil:
		return 0, 0, "", "", false, fmt.Errorf("reading a slack delivery: %w", scanErr)
	}
	return status, sequence, streamTS, note, true, nil
}

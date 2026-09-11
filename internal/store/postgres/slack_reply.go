package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

// The reply states, as the column stores them. Frozen, and exported so the enum gate can
// hold this file to them.
const (
	SlackReplyPending    = 1
	SlackReplyDelivering = 2
	SlackReplyDelivered  = 3
	SlackReplyFailed     = 4
)

// oweSlackReplies records an answer owed for every turn of a Slack conversation that has
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
func oweSlackReplies(ctx context.Context, pool *pgxpool.Pool, limit int) error {
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_reply
			(investigation_id, org_id, conversation_id, updated_at)
		SELECT i.investigation_id, i.org_id, i.conversation_id, now()
		  FROM investigation i
		  JOIN slack_conversation s
		    ON s.org_id = i.org_id AND s.conversation_id = i.conversation_id
		 WHERE i.conversation_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM slack_reply d
		                    WHERE d.investigation_id = i.investigation_id)
		 ORDER BY i.created_at
		 LIMIT $1
		ON CONFLICT (investigation_id) DO NOTHING`, limit); err != nil {
		return fmt.Errorf("recording owed slack replies: %w", err)
	}
	return nil
}

// ClaimSlackReplies leases the replies that are due.
//
// This cross-Organization sweep grants a unique token for each local delivery claim.
// Provider acceptance is outside the database transaction.
func (p *Database) ClaimSlackReplies(
	ctx context.Context, limit int, lease time.Duration,
) ([]slack.Reply, error) {
	// Every turn of a Slack conversation owes an answer, whether it was opened by
	// somebody speaking or by the drain behind a running turn.
	if err := oweSlackReplies(ctx, p.pool, limit); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		WITH claimed AS (
			UPDATE slack_reply
			   SET status       = $1,
			       lease_owner  = gen_random_uuid(),
			       leased_until = now() + $2::interval,
			       updated_at   = now()
			 WHERE investigation_id IN (
			       SELECT investigation_id
			         FROM slack_reply
			        WHERE status IN ($3, $1)
			          AND next_attempt_at <= now()
			          AND (leased_until IS NULL OR leased_until < now())
			        ORDER BY next_attempt_at
			        LIMIT $4
			          FOR UPDATE SKIP LOCKED)
			RETURNING investigation_id, org_id, conversation_id,
			          stream_ts, native, last_sequence, attempts, lease_owner, leased_until
		)
		SELECT c.investigation_id, c.org_id, s.integration_id, c.conversation_id,
		       s.channel_id, s.thread_ts, c.stream_ts, c.native, c.last_sequence,
		       c.attempts, c.lease_owner, c.leased_until
		FROM claimed c JOIN slack_conversation s
		  ON s.org_id = c.org_id AND s.conversation_id = c.conversation_id`,
		SlackReplyDelivering, lease.String(), SlackReplyPending, limit)
	if err != nil {
		return nil, fmt.Errorf("claiming slack replies: %w", err)
	}
	defer rows.Close()
	var claimed []slack.Reply
	for rows.Next() {
		var (
			one          slack.Reply
			organization string
		)
		if err := rows.Scan(&one.Investigation, &organization, &one.Integration,
			&one.Conversation, &one.Stream.Channel, &one.Stream.Thread,
			&one.Stream.TS, &one.Stream.Native,
			&one.LastSequence, &one.Attempts, &one.ClaimToken, &one.LeaseExpiresAt); err != nil {
			return nil, fmt.Errorf("scanning a slack reply: %w", err)
		}
		named, err := tenancy.NewOrganization(organization)
		if err != nil {
			return nil, fmt.Errorf(
				"a slack reply names an organization that is not a name: %w", err)
		}
		one.Organization = named
		claimed = append(claimed, one)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming slack replies: %w", err)
	}
	return claimed, nil
}

// AdvanceSlackReply saves acknowledged progress under the current claim.
func (p *Database) AdvanceSlackReply(
	ctx context.Context, organization tenancy.Organization, investigation, owner uuid.UUID,
	made slack.Progress,
) error {
	return p.withSlackReplyClaim(ctx, organization, investigation, owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
		UPDATE slack_reply
		   SET stream_ts     = CASE WHEN stream_ts = '' THEN $3 ELSE stream_ts END,
		       native     = CASE WHEN stream_ts = '' THEN $4 ELSE native END,
		       last_sequence = GREATEST(last_sequence, $5),
		       attempts      = 0,
		       note          = '',
		       updated_at    = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
			investigation, organization.String(), made.Stream.TS, made.Stream.Native,
			made.Sequence); err != nil {
			return fmt.Errorf("advancing a slack reply: %w", err)
		}
		return nil
	})
}

// RecordCollaborationWrite puts one reply into a customer's workspace on the audit record.
//
// A COLLABORATION write, and the word is the point: it is the only thing this product writes
// into a system it does not own, and it is deliberately distinct from an external read and
// firmly distinct from a production or remediation write, which remain unsupported. It is
// recorded once per turn, when the message appears, so "what did OpenCluster say in our Slack"
// has an answer that does not depend on Slack's own retention.
//
// The actor is the SYSTEM. Nobody pressed a button: a turn the worker picked up is the product
// answering a question somebody asked, and attributing it to that person would say they wrote
// what OpenCluster wrote.
func (p *Database) RecordCollaborationWrite(
	ctx context.Context, organization tenancy.Organization,
	integration uuid.UUID, where string,
) error {
	return p.RecordEvent(ctx, organization, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("control-plane"),
		Action:       audit.ActionCollaborationReplied,
		Target:       audit.Target{Kind: audit.TargetIntegration, ID: integration.String()},
		Outcome:      audit.OutcomeAllowed,
		// The channel, and nothing said in it. What OpenCluster answered is the
		// investigation's own record; repeating it here would be a second copy of a
		// customer's operational detail in a record kept for a different reason.
		Detail: audit.Detail{"surface": "slack", "channel": where},
	})
}

// CompleteSlackReply marks one delivered. Nothing claims it again.
func (p *Database) CompleteSlackReply(
	ctx context.Context, organization tenancy.Organization, investigation, owner uuid.UUID,
) error {
	return p.withSlackReplyClaim(ctx, organization, investigation, owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
		UPDATE slack_reply
		   SET status = $3, leased_until = NULL, lease_owner = NULL, updated_at = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
			investigation, organization.String(), SlackReplyDelivered); err != nil {
			return fmt.Errorf("completing a slack reply: %w", err)
		}
		return nil
	})
}

// RetrySlackReply schedules another attempt, or gives up.
//
// Giving up is TERMINAL for the reply and says nothing about the investigation, which has
// its own status and its own record. That separation is the point: a Slack outage must not be
// able to make a completed investigation look failed.
func (p *Database) RetrySlackReply(
	ctx context.Context, organization tenancy.Organization, investigation, owner uuid.UUID,
	at time.Time, note string, giveUp bool,
) error {
	return p.withSlackReplyClaim(ctx, organization, investigation, owner, func(tx pgx.Tx) error {
		status := SlackReplyPending
		if giveUp {
			status = SlackReplyFailed
		}
		if _, err := tx.Exec(ctx, `
		UPDATE slack_reply
		   SET status          = $3,
		       attempts        = attempts + 1,
		       next_attempt_at = $4,
		       note            = $5,
		       leased_until    = NULL,
		       lease_owner     = NULL,
		       updated_at      = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
			investigation, organization.String(), status, at.UTC(), note); err != nil {
			return fmt.Errorf("rescheduling a slack reply: %w", err)
		}
		return nil
	})
}

// SlackReplyState reports what one reply looks like, for the tests and for support. It
// answers false where the investigation owes no Slack answer.
func (p *Database) SlackReplyState(
	ctx context.Context, organization tenancy.Organization, investigation uuid.UUID,
) (status int, sequence int64, streamTS string, note string, found bool, err error) {
	pool, poolErr := p.Pool(organization)
	if poolErr != nil {
		return 0, 0, "", "", false, poolErr
	}
	scanErr := pool.QueryRow(ctx, `
		SELECT status, last_sequence, stream_ts, note
		  FROM slack_reply
		 WHERE investigation_id = $1 AND org_id = $2`,
		investigation, organization.String()).Scan(&status, &sequence, &streamTS, &note)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return 0, 0, "", "", false, nil
	case scanErr != nil:
		return 0, 0, "", "", false, fmt.Errorf("reading a slack reply: %w", scanErr)
	}
	return status, sequence, streamTS, note, true, nil
}

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
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

const (
	SlackReplyPending    = 1
	SlackReplyDelivering = 2
	SlackReplyDelivered  = 3
	SlackReplyFailed     = 4
)

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

func (p *Database) ClaimSlackReplies(
	ctx context.Context, limit int, lease time.Duration,
) ([]slack.Reply, error) {
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
		var one slack.Reply
		if err := rows.Scan(&one.Investigation, &one.Organization, &one.Integration,
			&one.Conversation, &one.Stream.Channel, &one.Stream.Thread,
			&one.Stream.TS, &one.Stream.Native,
			&one.LastSequence, &one.Attempts, &one.ClaimToken, &one.LeaseExpiresAt); err != nil {
			return nil, fmt.Errorf("scanning a slack reply: %w", err)
		}
		claimed = append(claimed, one)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming slack replies: %w", err)
	}
	return claimed, nil
}

func (p *Database) AdvanceSlackReply(
	ctx context.Context, organization uuid.UUID, investigation, owner uuid.UUID,
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
			investigation, organization, made.Stream.TS, made.Stream.Native,
			made.Sequence); err != nil {
			return fmt.Errorf("advancing a slack reply: %w", err)
		}
		return nil
	})
}

func (p *Database) RecordCollaborationWrite(
	ctx context.Context, organization uuid.UUID,
	integration uuid.UUID, where string,
) error {
	return p.RecordEvent(ctx, organization, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("control-plane"),
		Action:       audit.ActionCollaborationReplied,
		Target:       audit.Target{Kind: audit.TargetIntegration, ID: integration.String()},
		Outcome:      audit.OutcomeAllowed,
		Detail:       audit.Detail{"surface": "slack", "channel": where},
	})
}

func (p *Database) CompleteSlackReply(
	ctx context.Context, organization uuid.UUID, investigation, owner uuid.UUID,
) error {
	return p.withSlackReplyClaim(ctx, organization, investigation, owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
		UPDATE slack_reply
		   SET status = $3, leased_until = NULL, lease_owner = NULL, updated_at = now()
		 WHERE investigation_id = $1 AND org_id = $2`,
			investigation, organization, SlackReplyDelivered); err != nil {
			return fmt.Errorf("completing a slack reply: %w", err)
		}
		return nil
	})
}

func (p *Database) RetrySlackReply(
	ctx context.Context, organization uuid.UUID, investigation, owner uuid.UUID,
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
			investigation, organization, status, at.UTC(), note); err != nil {
			return fmt.Errorf("rescheduling a slack reply: %w", err)
		}
		return nil
	})
}

func (p *Database) ReleaseSlackReply(ctx context.Context, org uuid.UUID, id, owner uuid.UUID, at time.Time) error {
	return p.withSlackReplyClaim(ctx, org, id, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE slack_reply SET status = $3, lease_owner = NULL,
			leased_until = NULL, next_attempt_at = $4, updated_at = now()
			WHERE org_id = $1 AND investigation_id = $2`, org, id, SlackReplyPending, at.UTC())
		return err
	})
}

func (p *Database) withSlackReplyClaim(
	ctx context.Context, org uuid.UUID, id, owner uuid.UUID,
	write func(pgx.Tx) error,
) error {
	pool, err := p.poolForOrganization(org)
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
		WHERE org_id = $1 AND investigation_id = $2 FOR UPDATE`, org, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return slack.ErrReplyClaimLost
	}
	if err != nil {
		return fmt.Errorf("locking a slack reply: %w", err)
	}
	// Check expiry after locking because the wait may outlive the claim.
	var owned bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE(status = $4 AND lease_owner = $3
		AND leased_until > clock_timestamp(), false) FROM slack_reply
		WHERE org_id = $1 AND investigation_id = $2`, org, id, owner, SlackReplyDelivering).Scan(&owned); err != nil {
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

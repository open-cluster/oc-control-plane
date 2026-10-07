package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

type SlackMessage struct {
	Integration   uuid.UUID
	ContentDigest []byte
	Channel       string
	Thread        string
	MessageID     string
	Subject       string
	ActorID       string
	ActorDisplay  string
	Text          string
}

type SlackMessageOutcome struct {
	Duplicate    bool
	Conversation uuid.UUID
	Opened       bool
}

func (p *Database) RecordSlackMessage(
	ctx context.Context, organization uuid.UUID, said SlackMessage,
) (SlackMessageOutcome, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("beginning a slack message: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// Insert the claim first so concurrent redeliveries cannot both pass a read-then-write check.
	deliveryID := uuid.New()
	tag, err := transaction.Exec(ctx, `
		INSERT INTO webhook_delivery
			(delivery_id, org_id, integration_id, content_digest, provider_identity,
			 lifecycle_phase)
		VALUES ($1, $2, $3, $4, encode($4, 'hex'), '')
		ON CONFLICT (integration_id, provider_identity, lifecycle_phase)
		DO NOTHING`,
		deliveryID, organization, said.Integration, said.ContentDigest)
	if err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("recording a slack delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return SlackMessageOutcome{Duplicate: true}, nil
	}
	if err := reserveQueuedMessage(ctx, transaction, organization); err != nil {
		return SlackMessageOutcome{}, err
	}

	conversationID, opened, err := bindThread(ctx, transaction, organization, said)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	sequence, err := appendSlackMessage(ctx, transaction, organization, conversationID, said)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	if err := enqueueSlackMessageWork(ctx, transaction, organization,
		deliveryID, said.Integration, conversationID, sequence); err != nil {
		return SlackMessageOutcome{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("committing a slack message: %w", err)
	}
	return SlackMessageOutcome{Conversation: conversationID, Opened: opened}, nil
}

func bindThread(
	ctx context.Context, transaction pgx.Tx,
	organization uuid.UUID, said SlackMessage,
) (uuid.UUID, bool, error) {
	var existing uuid.UUID
	err := transaction.QueryRow(ctx, `
		SELECT conversation_id
		  FROM slack_conversation
		 WHERE integration_id = $1 AND channel_id = $2 AND thread_ts = $3
		   AND org_id = $4`,
		said.Integration, said.Channel, said.Thread, organization).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, fmt.Errorf("resolving a slack thread: %w", err)
	}

	opened := uuid.New()
	if _, err := transaction.Exec(ctx, `
		INSERT INTO conversation (conversation_id, org_id, surface, subject, created_by)
		VALUES ($1, $2, $3, $4, $5)`,
		opened, organization, int16(conversation.SurfaceSlack),
		said.Subject, said.ActorID); err != nil {
		return uuid.Nil, false, fmt.Errorf("opening a slack conversation: %w", err)
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO slack_conversation
			(conversation_id, org_id, integration_id, channel_id, thread_ts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (integration_id, channel_id, thread_ts) DO NOTHING`,
		opened, organization, said.Integration, said.Channel, said.Thread)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("binding a slack thread: %w", err)
	}
	if tag.RowsAffected() != 1 {
		if err := transaction.QueryRow(ctx, `
			SELECT conversation_id
			  FROM slack_conversation
			 WHERE integration_id = $1 AND channel_id = $2 AND thread_ts = $3
			   AND org_id = $4`,
			said.Integration, said.Channel, said.Thread, organization).Scan(&existing); err != nil {
			return uuid.Nil, false, fmt.Errorf("resolving a raced slack thread: %w", err)
		}
		return existing, false, nil
	}
	return opened, true, nil
}

func appendSlackMessage(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	conversationID uuid.UUID, said SlackMessage,
) (int64, error) {
	if _, err := lockConversation(ctx, transaction, organization, conversationID); err != nil {
		return 0, err
	}
	window, err := acceptedWindow(ctx, transaction, organization, conversationID, nil, conversation.DefaultIncidentWindowLead)
	if err != nil {
		return 0, err
	}
	var sequence int64
	if err := transaction.QueryRow(ctx, `
		INSERT INTO conversation_message (conversation_id, org_id, sequence, role,
		                                  actor_kind, actor_id, actor_display, text,
		                                  provider_channel_id, provider_message_id, window_from, window_until)
		SELECT $1, $2,
		       coalesce((SELECT max(sequence)
		                   FROM conversation_message
		                  WHERE org_id = $2 AND conversation_id = $1), 0) + 1,
		       $3, $4, $5, $6, $7, $8, $9, $10, $11
		RETURNING sequence`,
		conversationID, organization,
		int16(conversation.RolePerson), int16(conversation.ActorExternal),
		said.ActorID, said.ActorDisplay, said.Text, said.Channel, said.MessageID, window.From, window.Until).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("appending a slack message: %w", err)
	}
	if _, err := transaction.Exec(ctx, `
		UPDATE conversation
		   SET last_activity_at = now()
		 WHERE conversation_id = $1 AND org_id = $2`,
		conversationID, organization); err != nil {
		return 0, fmt.Errorf("stamping a slack conversation: %w", err)
	}
	return sequence, nil
}

func (p *Database) SlackMessageProviderReference(
	ctx context.Context, organization uuid.UUID, conversationID uuid.UUID, sequence int64,
) (channel, message, reference string, err error) {
	pool, poolErr := p.poolForOrganization(organization)
	if poolErr != nil {
		return "", "", "", poolErr
	}
	err = pool.QueryRow(ctx, `
		SELECT provider_channel_id, provider_message_id, source_reference
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3`,
		organization, conversationID, sequence).Scan(&channel, &message, &reference)
	if err != nil {
		return "", "", "", fmt.Errorf("reading slack message provider reference: %w", err)
	}
	return channel, message, reference, nil
}

func (p *Database) SetSlackMessageSourceReference(
	ctx context.Context, organization uuid.UUID, conversationID uuid.UUID,
	sequence int64, reference string, work SlackMessageWork,
) error {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE conversation_message AS message
		   SET source_reference = $4
		  FROM slack_message_work AS work
		 WHERE message.org_id = $1 AND message.conversation_id = $2 AND message.sequence = $3
		   AND work.org_id = $1 AND work.work_id = $5 AND work.status = 2
		   AND work.lease_owner = $6 AND work.lease_epoch = $7 AND work.lease_expires_at > now()`,
		organization, conversationID, sequence, reference,
		work.ID, work.LeaseOwner, work.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("recording slack message source reference: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrSlackMessageLeaseLost
	}
	return nil
}

func (p *Database) UnnamedSlackAuthors(
	ctx context.Context, organization uuid.UUID, conversationID uuid.UUID,
) ([]string, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT actor_id
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2
		   AND actor_kind = $3 AND actor_id <> '' AND actor_display = actor_id`,
		organization, conversationID, int16(conversation.ActorExternal))
	if err != nil {
		return nil, fmt.Errorf("reading unnamed slack authors: %w", err)
	}
	defer rows.Close()

	var unnamed []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			return nil, fmt.Errorf("scanning an unnamed slack author: %w", err)
		}
		unnamed = append(unnamed, actor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading unnamed slack authors: %w", err)
	}
	return unnamed, nil
}

func (p *Database) NameSlackAuthor(
	ctx context.Context, organization uuid.UUID, conversationID uuid.UUID,
	actor, display string,
) error {
	if display == "" || display == actor {
		return nil
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		UPDATE conversation_message
		   SET actor_display = $4
		 WHERE org_id = $1 AND conversation_id = $2
		   AND actor_kind = $5 AND actor_id = $3`,
		organization, conversationID, actor,
		conversation.Bounded(display, conversation.MaxActorDisplayLength),
		int16(conversation.ActorExternal)); err != nil {
		return fmt.Errorf("naming a slack author: %w", err)
	}
	return nil
}

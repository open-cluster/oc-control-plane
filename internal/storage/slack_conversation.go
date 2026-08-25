package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// A SLACK THREAD BECOMES A CONVERSATION, IN ONE TRANSACTION.
//
// The three things that must commit together are the delivery's idempotence claim, the
// thread-to-Conversation binding, and the message itself. Any two of them without the third
// is a state a customer can see: a claimed delivery with no message is a question OpenCluster
// silently dropped and the retry cannot recover, because the source has already been told it
// succeeded; a message without the claim is one answered twice on the next redelivery.
//
// Deduplication reuses the DELIVERY record the product already has rather than a Slack event
// table, so there is one idempotence story and one place to look when a customer says "it
// answered twice". A Slack retry carries the same body and is therefore already covered.
//
// Nothing here waits on a model, a repository, a cluster or an investigation. The turn is
// opened as an unclaimed record and the ordinary claiming worker takes it, which is what keeps
// acknowledgement inside Slack's timeout however long the investigation then takes.

// SlackMessage is one agent-directed message from a workspace, already authenticated,
// already resolved to its Integration and already judged to be something to answer.
type SlackMessage struct {
	// Integration is the installation the event resolved through, and the only authority
	// for the tenant everything in it belongs to.
	Integration uuid.UUID
	// BodyDigest is SHA-256 over the raw body as received. It is the idempotence identity,
	// and nothing else from the payload is stored on the delivery.
	BodyDigest []byte
	// Channel and Thread are Slack's identity for where this was said. Thread is the
	// message's own timestamp when it started no thread, which is the thread OpenCluster's
	// reply then creates.
	Channel string
	Thread  string
	// MessageID is Slack's timestamp identifier for this exact message. It is retained as
	// a safe provider reference so a worker can attach provenance after acknowledgement.
	MessageID string
	// Subject names the Conversation when this message opens one. It is derived from the
	// message rather than asked for, because nobody types a subject into a chat box.
	Subject string
	// Actor is who said it, in Slack's identifiers and in their display name. Recorded on
	// every message so that a shared thread stays attributable.
	ActorID      string
	ActorDisplay string
	// Text is what they said. UNTRUSTED for its whole life: it reaches a model as evidence
	// about what somebody typed, never as an instruction.
	Text string
}

// SlackMessageOutcome is what happened to one inbound message.
type SlackMessageOutcome struct {
	// Duplicate reports that this exact body was already accepted through this Integration,
	// so nothing was written a second time. It is a SUCCESS: a workspace retrying because
	// it never saw our answer has done nothing wrong, and the answer must let it stop.
	Duplicate bool
	// Conversation is the thread's conversation, whether this message opened it or joined
	// it.
	Conversation uuid.UUID
	// Opened reports that this message started the conversation rather than continuing one.
	Opened bool
}

// RecordSlackMessage claims the delivery, resolves the thread to its Conversation, and
// appends the message — all or nothing.
func (p *Database) RecordSlackMessage(
	ctx context.Context, organization tenancy.Organization, said SlackMessage,
) (SlackMessageOutcome, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("beginning a slack message: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The idempotence claim comes FIRST, so a redelivery that arrives while the first is
	// still committing loses the race at the database rather than at a read-then-write both
	// could pass.
	deliveryID := uuid.New()
	tag, err := transaction.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, signal_count)
		VALUES ($1, $2, $3, 1, $4, 0)
		ON CONFLICT (integration_id, body_digest) WHERE outcome = 1 DO NOTHING`,
		deliveryID, organization.String(), said.Integration, said.BodyDigest)
	if err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("recording a slack delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return SlackMessageOutcome{Duplicate: true}, nil
	}

	conversationID, opened, err := bindThread(ctx, transaction, organization, said)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	sequence, err := appendSlackMessage(ctx, transaction, organization, conversationID, said)
	if err != nil {
		return SlackMessageOutcome{}, err
	}
	if err := enqueueWebhookWork(ctx, transaction, organization, WebhookWorkSlack,
		deliveryID, said.Integration, uuid.Nil, conversationID, sequence); err != nil {
		return SlackMessageOutcome{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return SlackMessageOutcome{}, fmt.Errorf("committing a slack message: %w", err)
	}
	return SlackMessageOutcome{Conversation: conversationID, Opened: opened}, nil
}

// bindThread resolves this thread to its Conversation, opening one the first time.
//
// DETERMINISTIC. The binding is a lookup on the integration, the channel and the thread, and
// there is no inference anywhere near it: no similarity matching, no guessing which incident
// a thread is about. A Conversation is associated with an episode only when it was opened
// from one, and this path opens it from a mention, so it has none — which is the honest state
// rather than a guess that reads like knowledge.
func bindThread(
	ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, said SlackMessage,
) (uuid.UUID, bool, error) {
	var existing uuid.UUID
	err := transaction.QueryRow(ctx, `
		SELECT conversation_id
		  FROM slack_conversation
		 WHERE integration_id = $1 AND channel_id = $2 AND thread_ts = $3
		   AND org_id = $4`,
		said.Integration, said.Channel, said.Thread, organization.String()).Scan(&existing)
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
		opened, organization.String(), int16(conversation.SurfaceSlack),
		said.Subject, said.ActorID); err != nil {
		return uuid.Nil, false, fmt.Errorf("opening a slack conversation: %w", err)
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO slack_conversation
			(conversation_id, org_id, integration_id, channel_id, thread_ts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (integration_id, channel_id, thread_ts) DO NOTHING`,
		opened, organization.String(), said.Integration, said.Channel, said.Thread)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("binding a slack thread: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// Two messages in one thread arriving at once, and the other one won. Read the
		// binding it wrote rather than failing: both messages belong in one conversation,
		// which is the whole point of the binding being unique.
		if err := transaction.QueryRow(ctx, `
			SELECT conversation_id
			  FROM slack_conversation
			 WHERE integration_id = $1 AND channel_id = $2 AND thread_ts = $3
			   AND org_id = $4`,
			said.Integration, said.Channel, said.Thread, organization.String()).Scan(&existing); err != nil {
			return uuid.Nil, false, fmt.Errorf("resolving a raced slack thread: %w", err)
		}
		return existing, false, nil
	}
	return opened, true, nil
}

// appendSlackMessage writes the message at the next sequence and stamps the conversation.
//
// The author is an EXTERNAL actor rather than a principal. A person speaking in a workspace
// may hold no OpenCluster account at all, and recording their Slack identity as though it
// were one would be inventing a principal — while dropping the identity would lose attribution
// in exactly the case it matters, a thread several people are working in.
func appendSlackMessage(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	conversationID uuid.UUID, said SlackMessage,
) (int64, error) {
	var sequence int64
	if err := transaction.QueryRow(ctx, `
		INSERT INTO conversation_message (conversation_id, org_id, sequence, role,
		                                  actor_kind, actor_id, actor_display, text,
		                                  provider_channel_id, provider_message_id)
		SELECT $1, $2,
		       coalesce((SELECT max(sequence)
		                   FROM conversation_message
		                  WHERE org_id = $2 AND conversation_id = $1), 0) + 1,
		       $3, $4, $5, $6, $7, $8, $9
		RETURNING sequence`,
		conversationID, organization.String(),
		int16(conversation.RolePerson), int16(conversation.ActorExternal),
		said.ActorID, said.ActorDisplay, said.Text, said.Channel, said.MessageID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("appending a slack message: %w", err)
	}
	if _, err := transaction.Exec(ctx, `
		UPDATE conversation
		   SET last_activity_at = now()
		 WHERE conversation_id = $1 AND org_id = $2`,
		conversationID, organization.String()); err != nil {
		return 0, fmt.Errorf("stamping a slack conversation: %w", err)
	}
	return sequence, nil
}

// SlackMessageProviderReference reports the safe provider identifiers retained for one
// accepted message. It never returns the message body or a credential.
func (p *Database) SlackMessageProviderReference(
	ctx context.Context, organization tenancy.Organization, conversationID uuid.UUID, sequence int64,
) (channel, message, reference string, err error) {
	pool, poolErr := p.Pool(organization)
	if poolErr != nil {
		return "", "", "", poolErr
	}
	err = pool.QueryRow(ctx, `
		SELECT provider_channel_id, provider_message_id, source_reference
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3`,
		organization.String(), conversationID, sequence).Scan(&channel, &message, &reference)
	if err != nil {
		return "", "", "", fmt.Errorf("reading slack message provider reference: %w", err)
	}
	return channel, message, reference, nil
}

// SetSlackMessageSourceReference records a scope-free navigation URL derived after the
// acknowledgement path has completed.
func (p *Database) SetSlackMessageSourceReference(
	ctx context.Context, organization tenancy.Organization, conversationID uuid.UUID,
	sequence int64, reference string, work WebhookWork,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE conversation_message AS message
		   SET source_reference = $4
		  FROM webhook_work AS work
		 WHERE message.org_id = $1 AND message.conversation_id = $2 AND message.sequence = $3
		   AND work.org_id = $1 AND work.work_id = $5 AND work.status = 2
		   AND work.lease_owner = $6 AND work.lease_epoch = $7 AND work.lease_expires_at > now()`,
		organization.String(), conversationID, sequence, reference,
		work.ID, work.LeaseOwner, work.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("recording slack message source reference: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrWebhookWorkLeaseLost
	}
	return nil
}

// SlackThreadOf reports where a conversation's replies belong, so a delivery worker can
// answer in the thread the question was asked in. It answers false for a conversation that
// did not come from Slack.
func (p *Database) SlackThreadOf(
	ctx context.Context, organization tenancy.Organization, conversationID uuid.UUID,
) (channel string, thread string, integration uuid.UUID, found bool, err error) {
	pool, poolErr := p.Pool(organization)
	if poolErr != nil {
		return "", "", uuid.Nil, false, poolErr
	}
	scanErr := pool.QueryRow(ctx, `
		SELECT channel_id, thread_ts, integration_id
		  FROM slack_conversation
		 WHERE conversation_id = $1 AND org_id = $2`,
		conversationID, organization.String()).Scan(&channel, &thread, &integration)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return "", "", uuid.Nil, false, nil
	case scanErr != nil:
		return "", "", uuid.Nil, false,
			fmt.Errorf("reading a slack thread binding: %w", scanErr)
	}
	return channel, thread, integration, true, nil
}

// UnnamedSlackAuthors reports the Slack identities in one conversation that are still recorded
// under their raw identifier.
//
// It exists because resolving a name costs a call to the vendor, and the ONE place that must
// not make one is the endpoint that acknowledges an event: Slack retries anything it is not
// answered inside three seconds, and a users.info on that path is a vendor outage becoming a
// retry storm. So the message is recorded under the identity it arrived with, and the worker
// that answers — which already holds the credential and is under no deadline anybody sees —
// resolves it afterwards.
func (p *Database) UnnamedSlackAuthors(
	ctx context.Context, organization tenancy.Organization, conversationID uuid.UUID,
) ([]string, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT actor_id
		  FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2
		   AND actor_kind = $3 AND actor_id <> '' AND actor_display = actor_id`,
		organization.String(), conversationID, int16(conversation.ActorExternal))
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

// NameSlackAuthor records what one Slack identity is called, on every message they wrote in
// this conversation.
//
// The identity is never replaced, only named beside itself. Attribution has to survive a
// display name changing or a name that cannot be resolved at all, and the identifier is the
// half that does.
func (p *Database) NameSlackAuthor(
	ctx context.Context, organization tenancy.Organization, conversationID uuid.UUID,
	actor, display string,
) error {
	if display == "" || display == actor {
		return nil
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		UPDATE conversation_message
		   SET actor_display = $4
		 WHERE org_id = $1 AND conversation_id = $2
		   AND actor_kind = $5 AND actor_id = $3`,
		organization.String(), conversationID, actor,
		conversation.Bounded(display, conversation.MaxActorDisplayLength),
		int16(conversation.ActorExternal)); err != nil {
		return fmt.Errorf("naming a slack author: %w", err)
	}
	return nil
}

package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var errConversationOrigin = errors.New("conversation provider origin is missing or inconsistent")

func (p *Database) ConversationOrigin(ctx context.Context, organization uuid.UUID, id uuid.UUID) (*investigation.ConversationOrigin, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}
	return conversationOrigin(ctx, pool, organization, id, false)
}

func conversationOrigin(ctx context.Context, on querier, organization, id uuid.UUID, requireVerified bool) (*investigation.ConversationOrigin, error) {
	var source conversation.Source
	var integration *uuid.UUID
	var channel, thread string
	err := on.QueryRow(ctx, `SELECT c.source, i.integration_id,
		COALESCE(s.channel_id, ''), COALESCE(s.thread_ts, '')
		FROM conversation c
		LEFT JOIN slack_conversation s ON s.org_id = c.org_id AND s.conversation_id = c.conversation_id
		LEFT JOIN integration i ON i.org_id = c.org_id AND i.integration_id = s.integration_id
		    AND (NOT $3 OR (NOT i.disconnected AND i.verification_status = 'verified' AND i.verified_at IS NOT NULL
		        AND EXISTS (SELECT 1 FROM integration_installation installed
		                     WHERE installed.org_id = i.org_id AND installed.integration_id = i.integration_id)))
		WHERE c.org_id = $1 AND c.conversation_id = $2`, organization, id, requireVerified).
		Scan(&source, &integration, &channel, &thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, conversation.ErrUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("reading Conversation origin: %w", err)
	}
	if source == conversation.SourceWeb && integration == nil && channel == "" && thread == "" {
		return nil, nil
	}
	if source != conversation.SourceSlack || integration == nil || *integration == uuid.Nil || channel == "" || thread == "" {
		return nil, errConversationOrigin
	}
	return &investigation.ConversationOrigin{
		IntegrationID: *integration,
		Channel:       channel,
		Thread:        thread,
	}, nil
}

func (p *Database) InvestigationMessages(ctx context.Context, org uuid.UUID, conversationID, id uuid.UUID) ([]investigation.AssignedMessage, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM investigation
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3)`,
		org, conversationID, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, investigation.ErrUnknown
	}
	rows, err := pool.Query(ctx, `SELECT sequence, author_name, created_at, text
		FROM conversation_message WHERE org_id = $1 AND conversation_id = $2
		AND investigation_id = $3 AND role = 1 ORDER BY sequence LIMIT $4`,
		org, conversationID, id, maxQueuedMessages+1)
	if err != nil {
		return nil, fmt.Errorf("reading assigned Messages: %w", err)
	}
	defer rows.Close()
	var messages []investigation.AssignedMessage
	for rows.Next() {
		var message investigation.AssignedMessage
		if err := rows.Scan(&message.Sequence, &message.Author, &message.CreatedAt, &message.Text); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(messages) > maxQueuedMessages {
		return nil, fmt.Errorf("assigned Message batch exceeds %d", maxQueuedMessages)
	}
	return messages, nil
}

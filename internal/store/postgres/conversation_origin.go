package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// ConversationOrigin reads the provider resource that constrains this Conversation.
func (p *Database) ConversationOrigin(ctx context.Context, organization tenancy.Organization, id uuid.UUID) (*investigation.ConversationOrigin, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	var surface conversation.Surface
	var integration *uuid.UUID
	var channel, thread string
	err = pool.QueryRow(ctx, `SELECT c.surface, i.integration_id,
		COALESCE(s.channel_id, ''), COALESCE(s.thread_ts, '')
		FROM conversation c
		LEFT JOIN slack_conversation s ON s.org_id = c.org_id AND s.conversation_id = c.conversation_id
		LEFT JOIN integration i ON i.org_id = c.org_id AND i.integration_id = s.integration_id
		WHERE c.org_id = $1 AND c.conversation_id = $2`, organization.String(), id).
		Scan(&surface, &integration, &channel, &thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, conversation.ErrUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("reading Conversation origin: %w", err)
	}
	if surface == conversation.SurfaceWeb && integration == nil && channel == "" && thread == "" {
		return nil, nil
	}
	if surface != conversation.SurfaceSlack || integration == nil || *integration == uuid.Nil || channel == "" || thread == "" {
		return nil, errors.New("Conversation provider origin is missing or inconsistent")
	}
	return &investigation.ConversationOrigin{IntegrationID: *integration, Channel: channel, Thread: thread}, nil
}

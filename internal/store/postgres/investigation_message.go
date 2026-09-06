package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// InvestigationMessages reads only the person Messages assigned to this exact turn.
func (p *Database) InvestigationMessages(ctx context.Context, org tenancy.Organization, conversationID, id uuid.UUID) ([]investigation.AssignedMessage, error) {
	pool, err := p.Pool(org)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM investigation
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3)`,
		org.String(), conversationID, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, investigation.ErrUnknown
	}
	rows, err := pool.Query(ctx, `SELECT sequence, actor_display, created_at, text
		FROM conversation_message WHERE org_id = $1 AND conversation_id = $2
		AND investigation_id = $3 AND role = 1 ORDER BY sequence LIMIT $4`,
		org.String(), conversationID, id, maxQueuedMessages+1)
	if err != nil {
		return nil, fmt.Errorf("reading assigned Messages: %w", err)
	}
	defer rows.Close()
	var messages []investigation.AssignedMessage
	for rows.Next() {
		var message investigation.AssignedMessage
		if err := rows.Scan(&message.Sequence, &message.Actor, &message.CreatedAt, &message.Text); err != nil {
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

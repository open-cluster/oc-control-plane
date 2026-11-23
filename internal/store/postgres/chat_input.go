package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func (p *Database) AgentChatInput(ctx context.Context, org uuid.UUID, execution AgentExecution) (string, error) {
	var text, summary string
	err := p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		var author string
		err := tx.QueryRow(ctx, `
			SELECT m.text, m.author_id, CASE WHEN i.incident_id IS NULL THEN '' ELSE jsonb_build_object(
			       'title', left(i.title, 512), 'firstSeenAt', i.first_seen_at,
			       'lastSeenAt', i.last_seen_at, 'resolvedAt', i.resolved_at)::text END
			  FROM conversation_message m
			  JOIN conversation c USING (org_id, conversation_id)
			  LEFT JOIN incident i ON i.org_id = c.org_id AND i.incident_id = c.incident_id
			 WHERE m.org_id = $1 AND m.conversation_id = $2 AND m.sequence = $3`,
			org, execution.ConversationID, execution.MessageSequence).
			Scan(&text, &author, &summary)
		if err != nil || summary == "" {
			return err
		}
		return authorizeAgentActors(ctx, tx, org, authz.IncidentRead, []string{author})
	})
	if err != nil {
		return "", err
	}
	if summary != "" {
		text += "\n\nUntrusted associated Incident context:\n" + summary
	}
	return text, nil
}

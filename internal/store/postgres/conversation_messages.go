package storage

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

const conversationMessageColumns = `m.sequence, m.role, m.author_id, m.author_name, m.text, m.source_reference,
	m.investigation_id, m.created_at, m.window_from, m.window_until, e.execution_id`

const visibleConversationMessages = `
	FROM conversation_message m
	JOIN conversation c USING (org_id, conversation_id)
	LEFT JOIN investigation i ON i.org_id = m.org_id AND i.investigation_id = m.investigation_id
	LEFT JOIN agent_execution e ON e.org_id = m.org_id AND e.conversation_id = m.conversation_id
	  AND e.message_sequence = m.sequence AND m.role = 1
	WHERE m.org_id = $1 AND m.conversation_id = $2
	  AND c.deleted_at IS NULL
	  AND (c.source <> 'web' OR m.role <> 2 OR ` + webInvestigationSource + `)`

func (p *Database) ConversationMessages(ctx context.Context, org, id uuid.UUID, limit int, cursor string) (conversation.MessagePage, error) {
	if _, err := p.WebConversation(ctx, org, id); err != nil {
		return conversation.MessagePage{}, err
	}
	scope := "conversation-messages/" + org.String() + "/" + id.String()
	position, cursorID, err := decodeSortCursor(cursor, scope)
	if err != nil {
		return conversation.MessagePage{}, conversation.ErrBadCursor
	}
	var after int64
	if cursorID != nil {
		after, err = strconv.ParseInt(position, 10, 64)
		if err != nil || after < 1 || *cursorID != id {
			return conversation.MessagePage{}, conversation.ErrBadCursor
		}
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return conversation.MessagePage{}, err
	}
	limit = pageLimit(limit)
	messages, err := readConversationMessages(ctx, pool, limit+1,
		`SELECT `+conversationMessageColumns+visibleConversationMessages+
			` AND c.source = 'web' AND m.sequence > $3 ORDER BY m.sequence LIMIT $4`,
		org, id, after, limit+1)
	if err != nil {
		return conversation.MessagePage{}, err
	}
	page := conversation.MessagePage{Messages: messages}
	if len(messages) > limit {
		page.Messages = messages[:limit]
		page.Next = encodeSortCursor(scope, strconv.FormatInt(messages[limit-1].Sequence, 10), id)
	}
	return page, nil
}

func readConversationMessages(ctx context.Context, queries querier, limit int, query string, args ...any) ([]conversation.Message, error) {
	rows, err := queries.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading a Conversation's Messages: %w", err)
	}
	defer rows.Close()
	messages := make([]conversation.Message, 0, limit)
	for rows.Next() {
		var execution uuid.NullUUID
		message, err := scanMessage(rows, &execution)
		if err != nil {
			return nil, err
		}
		message.ExecutionID = execution.UUID
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

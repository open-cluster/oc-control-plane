package storage

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

func (p *Database) ConversationTurns(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, limit int, cursor string,
) (conversation.TurnPage, error) {
	if _, err := p.Conversation(ctx, organization, id); err != nil {
		return conversation.TurnPage{}, err
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return conversation.TurnPage{}, err
	}
	return readConversationTurns(ctx, pool, organization, id, limit, cursor)
}

func readConversationTurns(
	ctx context.Context, queries querier, organization tenancy.Organization, id uuid.UUID, limit int, cursor string,
) (conversation.TurnPage, error) {
	scope := "conversation-turns/" + organization.String() + "/" + id.String() + "/turn,investigationId"
	value, afterID, err := decodeSortCursor(cursor, scope)
	if err != nil {
		return conversation.TurnPage{}, conversation.ErrBadCursor
	}
	var after int64
	if afterID != nil {
		after, err = strconv.ParseInt(value, 10, 32)
		if err != nil || after < 1 || *afterID == uuid.Nil {
			return conversation.TurnPage{}, conversation.ErrBadCursor
		}
	}
	limit = pageLimit(limit)
	rows, err := queries.Query(ctx, `
		SELECT investigation_id, turn, status, coalesce(conclusion->>'summary', ''),
		       stopped_by, error, created_at, concluded_at
		  FROM investigation
		 WHERE org_id = $1 AND conversation_id = $2
		   AND ($3::integer = 0 OR (turn, investigation_id) > ($3, $4::uuid))
		 ORDER BY turn, investigation_id
		 LIMIT $5`, organization.String(), id, after, afterID, limit+1)
	if err != nil {
		return conversation.TurnPage{}, fmt.Errorf("reading conversation turns: %w", err)
	}
	defer rows.Close()
	page := conversation.TurnPage{Turns: make([]conversation.Turn, 0, limit)}
	for rows.Next() {
		var turn conversation.Turn
		var status int16
		var concludedAt *time.Time
		if err := rows.Scan(&turn.InvestigationID, &turn.Ordinal, &status, &turn.Answer,
			&turn.StoppedBy, &turn.Error, &turn.CreatedAt, &concludedAt); err != nil {
			return conversation.TurnPage{}, fmt.Errorf("scanning conversation turn: %w", err)
		}
		turn.Status = investigationStatusWord(status)
		if concludedAt != nil {
			turn.ConcludedAt = *concludedAt
		}
		page.Turns = append(page.Turns, turn)
	}
	if err := rows.Err(); err != nil {
		return conversation.TurnPage{}, fmt.Errorf("reading conversation turns: %w", err)
	}
	if len(page.Turns) > limit {
		page.Turns = page.Turns[:limit]
		last := page.Turns[limit-1]
		page.Next = encodeSortCursor(scope, strconv.Itoa(last.Ordinal), last.InvestigationID)
	}
	return page, nil
}

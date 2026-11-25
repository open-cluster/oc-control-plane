package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const maxEventPage = 500

const slackInvestigationOwner = `EXISTS (
	SELECT 1 FROM investigation i
	JOIN conversation c ON c.org_id = i.org_id AND c.conversation_id = i.conversation_id
	WHERE i.org_id = $1 AND i.investigation_id = $2 AND i.turn IS NOT NULL AND c.source = 'slack'
)`

func appendSlackInvestigationEvent(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, payload investigation.EventPayload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding Slack Investigation event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
		SELECT $1, $2, coalesce(max(sequence), 0) + 1, $3, $4
		  FROM investigation_event
		 WHERE org_id = $1 AND investigation_id = $2
		HAVING `+slackInvestigationOwner,
		org, id, int16(payload.EventType()), encoded); err != nil {
		return fmt.Errorf("recording Slack Investigation event: %w", err)
	}
	return nil
}

func (p *Database) Events(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
	after int64, limit int,
) ([]investigation.Event, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxEventPage {
		limit = maxEventPage
	}
	rows, err := pool.Query(ctx, `
		SELECT v.sequence, v.at, v.type, v.payload
		  FROM investigation_event v
		 WHERE v.org_id = $1 AND v.investigation_id = $2 AND v.sequence > $3
		   AND v.type NOT IN (5, 8, 10)
		 ORDER BY v.sequence
		 LIMIT $4`, organization, id, after, limit)
	if err != nil {
		return nil, fmt.Errorf("reading investigation events: %w", err)
	}
	defer rows.Close()

	events := make([]investigation.Event, 0, limit)
	for rows.Next() {
		var (
			event     investigation.Event
			eventType int16
			payload   []byte
		)
		if err := rows.Scan(&event.Sequence, &event.At, &eventType,
			&payload); err != nil {
			return nil, fmt.Errorf("scanning an investigation event: %w", err)
		}
		event.Type = investigation.EventType(eventType)
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &event.Payload); err != nil {
				return nil, fmt.Errorf("decoding an event payload: %w", err)
			}
		}
		if event.Payload == nil {
			event.Payload = map[string]any{}
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading investigation events: %w", err)
	}
	return events, nil
}

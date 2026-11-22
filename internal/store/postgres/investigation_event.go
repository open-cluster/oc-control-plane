package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const maxEventPage = 500

func (p *Database) AppendEvent(
	ctx context.Context, organization uuid.UUID, id uuid.UUID, token uuid.UUID,
	event investigation.Event,
) error {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(orEmptyPayload(event.Payload))
	if err != nil {
		return fmt.Errorf("encoding an event payload: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockInvestigation(ctx, tx, organization, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO investigation_event (investigation_id, org_id, sequence, at, type,
		                                 payload)
		SELECT $1, $2,
		       (SELECT coalesce(max(sequence), 0) + 1 FROM investigation_event WHERE investigation_id = $1 AND org_id = $2),
		       $3, $4, $5
		  FROM investigation
		 WHERE investigation_id = $1 AND org_id = $2 AND status = 1
		 AND lease_token = $6 AND lease_expires_at > clock_timestamp()`,
		id, organization, event.At, int16(event.Type), payload, token)
	if err != nil {
		return fmt.Errorf("appending an investigation event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return investigation.ErrAlreadyEnded
	}
	return tx.Commit(ctx)
}

func (p *Database) Events(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
	after int64, limit int,
) ([]investigation.Event, error) {
	return p.events(ctx, organization, id, after, limit, false)
}

func (p *Database) WebEvents(
	ctx context.Context, organization, id uuid.UUID, after int64, limit int,
) ([]investigation.Event, error) {
	return p.events(ctx, organization, id, after, limit, true)
}

func (p *Database) events(
	ctx context.Context, organization, id uuid.UUID, after int64, limit int, webOnly bool,
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
		  JOIN investigation i ON i.org_id = v.org_id AND i.investigation_id = v.investigation_id
		 WHERE v.org_id = $1 AND v.investigation_id = $2 AND v.sequence > $3
		   AND v.type NOT IN (5, 8, 10) AND (NOT $5 OR `+webInvestigationSource+`)
		 ORDER BY v.sequence
		 LIMIT $4`, organization, id, after, limit, webOnly)
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

func orEmptyPayload(payload map[string]any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	return payload
}

package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The investigation event stream's persistence. It is a table rather than a broadcast
// because persistence is what makes reconnect and replica change THE SAME code path: a
// reader that lost its connection and one that landed on a different process both ask for
// what comes after a sequence, and both are answered from here.
var (
	_ investigation.EventSink   = (*Placements)(nil)
	_ investigation.EventReader = (*Placements)(nil)
)

// maxEventPage bounds one replay read, so a long investigation is drained in pages rather
// than in one answer nobody sized.
const maxEventPage = 500

// AppendEvent writes one event at the sequence it carries.
//
// The insert is unguarded on purpose: the primary key (investigation, sequence) IS the
// guard. The lease makes one writer, so a second row at the same position means two
// processes believed they held it, and refusing the write is how that stops being silent.
func (p *Placements) AppendEvent(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	event investigation.Event,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(orEmptyPayload(event.Payload))
	if err != nil {
		return fmt.Errorf("encoding an event payload: %w", err)
	}
	if _, err = pool.Exec(ctx, `
		INSERT INTO investigation_event (investigation_id, org_id, sequence, at, type,
		                                 payload)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, organization.String(), event.Sequence, event.At, int16(event.Type),
		payload); err != nil {
		return fmt.Errorf("appending an investigation event: %w", err)
	}
	return nil
}

// Events reports this investigation's events after a sequence, in order.
//
// It is scoped by organization in the WHERE clause like every other read here, so an
// identifier from another tenant returns nothing rather than somebody else's stream. The
// caller turns an empty answer for an investigation it could not read into not-found; this
// read does not distinguish "no events yet" from "not yours", and does not need to.
func (p *Placements) Events(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	after int64, limit int,
) ([]investigation.Event, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxEventPage {
		limit = maxEventPage
	}
	rows, err := pool.Query(ctx, `
		SELECT sequence, at, type, payload
		  FROM investigation_event
		 WHERE org_id = $1 AND investigation_id = $2 AND sequence > $3
		 ORDER BY sequence
		 LIMIT $4`, organization.String(), id, after, limit)
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

// orEmptyPayload renders a payload for the JSONB column. A nil map would encode as null,
// and a reader would have to tell null from an absent structure for no reason.
func orEmptyPayload(payload map[string]any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	return payload
}

package storage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

// acceptedWindow runs under the Conversation lock shared by admission and drain.
func acceptedWindow(ctx context.Context, tx pgx.Tx, org tenancy.Organization, id uuid.UUID,
	requested *conversation.Window, lead time.Duration,
) (conversation.Window, error) {
	if requested != nil {
		normalized := requested.Normalized()
		requested = &normalized
	}
	if requested != nil && !requested.Valid(time.Now()) {
		return conversation.Window{}, conversation.ErrInvalidWindow
	}
	var from, until *time.Time
	err := tx.QueryRow(ctx, `SELECT window_from, window_until FROM conversation_message
		WHERE org_id = $1 AND conversation_id = $2 AND investigation_id IS NULL AND role = 1
		ORDER BY sequence LIMIT 1`, org.String(), id).Scan(&from, &until)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return conversation.Window{}, err
	}
	if from != nil {
		if requested != nil && (!requested.From.Equal(*from) || !requested.Until.Equal(*until)) {
			return conversation.Window{}, conversation.ErrWindowConflict
		}
		return conversation.Window{From: *from, Until: *until}, nil
	}
	if requested != nil {
		return *requested, nil
	}
	var incident *uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT incident_id FROM conversation WHERE org_id = $1 AND conversation_id = $2`, org.String(), id).Scan(&incident); err != nil {
		return conversation.Window{}, err
	}
	f, u, err := turnWindow(ctx, tx, org, incident, lead)
	if err != nil {
		return conversation.Window{}, err
	}
	window := (conversation.Window{From: f, Until: u}).Normalized()
	if !window.Valid(time.Now()) {
		return conversation.Window{}, conversation.ErrInvalidWindow
	}
	return window, nil
}

package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// LocalPasswordHash reads only the authenticated User's local verifier.
func (p *Database) LocalPasswordHash(ctx context.Context, principal authz.Principal) (string, error) {
	user, err := uuid.Parse(principal.ID())
	if err != nil || principal.Kind() != authz.KindUser {
		return "", ErrLocalCredentialUnknown
	}
	var encoded string
	err = p.pool.QueryRow(ctx, `SELECT password_hash FROM local_password c JOIN app_user u USING (user_id)
		WHERE u.user_id = $1 AND u.issuer = $2 AND u.disabled_at IS NULL`, user, LocalIssuer).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLocalCredentialUnknown
	}
	if err != nil {
		return "", fmt.Errorf("reading local verifier: %w", err)
	}
	return encoded, nil
}

// ChangeLocalPassword atomically replaces a reauthenticated User's verifier and ends all sessions.
func (p *Database) ChangeLocalPassword(ctx context.Context, principal authz.Principal, previous, replacement string) error {
	user, err := uuid.Parse(principal.ID())
	if err != nil || principal.Kind() != authz.KindUser {
		return ErrLocalCredentialUnknown
	}
	return p.replaceLocalPassword(ctx, user, &previous, replacement, audit.Event{
		Actor: principal.Actor(), Action: audit.ActionLocalPasswordChanged,
		SourceAddress: principal.SourceAddress(), RequestID: principal.RequestID(),
	})
}

// RecoverLocalPassword uses deployment database authority to recover an existing local account.
func (p *Database) RecoverLocalPassword(ctx context.Context, user uuid.UUID, replacement string) error {
	if user == uuid.Nil {
		return ErrLocalCredentialUnknown
	}
	return p.replaceLocalPassword(ctx, user, nil, replacement, audit.Event{
		Actor: audit.System("deployment operator"), Action: audit.ActionLocalPasswordRecovered,
	})
}

func (p *Database) replaceLocalPassword(ctx context.Context, user uuid.UUID, previous *string, replacement string, event audit.Event) error {
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password change: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	tag, err := transaction.Exec(ctx, `UPDATE local_password SET password_hash = $3, updated_at = now()
		WHERE user_id = $1 AND ($2::text IS NULL OR password_hash = $2)
		AND EXISTS (SELECT 1 FROM app_user WHERE user_id = $1 AND issuer = $4 AND disabled_at IS NULL)`, user, previous, replacement, LocalIssuer)
	if err != nil {
		return fmt.Errorf("changing local password: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLocalCredentialUnknown
	}
	if _, err = transaction.Exec(ctx, `UPDATE session SET revoked_at = now(), revoked_by = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, user, event.Actor.ID); err != nil {
		return fmt.Errorf("revoking User sessions: %w", err)
	}
	event.Target = audit.Target{Kind: audit.TargetUser, ID: user.String()}
	event.Outcome = audit.OutcomeAllowed
	if err = writeEvent(ctx, transaction, event); err != nil {
		return err
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit password change: %w", err)
	}
	return nil
}

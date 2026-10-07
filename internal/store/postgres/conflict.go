package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type SessionConflict struct {
	DetectedAt    time.Time
	DistinctHosts int
}

func (p *Database) RecordSessionConflict(
	ctx context.Context,
	organization uuid.UUID,
	registrationID uuid.UUID,
	distinctHosts int,
) error {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("recording a session conflict: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	tag, err := transaction.Exec(ctx, `
		UPDATE relay_registration
		   SET session_conflict_at    = now(),
		       -- The high-water mark, not the latest reading. A conflict that involved two
		       -- hosts an hour ago is still a conflict that involved two hosts, and a later
		       -- quieter sighting must not talk an operator out of it.
		       session_conflict_hosts = GREATEST(session_conflict_hosts, $3)
		 WHERE registration_id = $1
		   AND org_id    = $2`,
		registrationID, organization, distinctHosts)
	if err != nil {
		return fmt.Errorf("recording a session conflict: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("recording a session conflict: registration %s not found", registrationID)
	}

	if err = writeEvent(ctx, transaction, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("relay session guard"),
		Action:       audit.ActionRelaySessionConflictDetected,
		Target:       audit.Target{Kind: audit.TargetRelay, ID: registrationID.String()},
		Outcome:      audit.OutcomeAllowed,
		Detail:       audit.Detail{"distinctHosts": distinctHosts},
	}); err != nil {
		return err
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("recording a session conflict: %w", err)
	}
	return nil
}

func (p *Database) SessionConflict(
	ctx context.Context, organization uuid.UUID, registrationID uuid.UUID,
) (SessionConflict, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return SessionConflict{}, err
	}

	var (
		detectedAt *time.Time
		hosts      int
	)
	err = pool.QueryRow(ctx, `
		SELECT session_conflict_at, session_conflict_hosts
		  FROM relay_registration
		 WHERE registration_id = $1 AND org_id = $2`,
		registrationID, organization).Scan(&detectedAt, &hosts)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionConflict{}, nil
	}
	if err != nil {
		return SessionConflict{}, fmt.Errorf("reading a session conflict: %w", err)
	}
	if detectedAt == nil {
		return SessionConflict{}, nil
	}
	return SessionConflict{DetectedAt: *detectedAt, DistinctHosts: hosts}, nil
}

type ConflictWithdrawal int

const (
	WithdrawalRelayUnknown ConflictWithdrawal = iota + 1
	WithdrawalNothingMarked
	WithdrawalRecorded
)

func (p *Database) ClearSessionConflict(
	ctx context.Context,
	principal authz.Principal,
	organization uuid.UUID,
	registrationID uuid.UUID,
) (ConflictWithdrawal, error) {
	if principal.Organization() != organization {
		return 0, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	tag, err := transaction.Exec(ctx, `
		UPDATE relay_registration
		   SET session_conflict_at    = NULL,
		       session_conflict_hosts = 0
		 WHERE registration_id     = $1
		   AND org_id        = $2
		   AND session_conflict_at IS NOT NULL`,
		registrationID, organization)
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return p.explainUnwithdrawn(ctx, organization, registrationID)
	}

	if err = writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionRelaySessionConflictCleared,
		Target:        audit.Target{Kind: audit.TargetRelay, ID: registrationID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
		Detail: audit.Detail{
			"severity": "warning",
			"effect":   "a relay credential-theft finding was destroyed",
		},
	}); err != nil {
		return 0, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	return WithdrawalRecorded, nil
}

func (p *Database) explainUnwithdrawn(
	ctx context.Context, organization uuid.UUID, registrationID uuid.UUID,
) (ConflictWithdrawal, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}
	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT true FROM relay_registration
		 WHERE registration_id = $1 AND org_id = $2`,
		registrationID, organization).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return WithdrawalRelayUnknown, nil
	}
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	return WithdrawalNothingMarked, nil
}

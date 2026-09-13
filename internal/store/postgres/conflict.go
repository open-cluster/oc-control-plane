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
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// SessionConflict is what the control plane has seen of two parties competing for one relay
// identity. A zero DetectedAt means it has seen none.
type SessionConflict struct {
	DetectedAt time.Time
	// DistinctHosts is how many hosts were seen taking the session. More than one is the
	// credential-theft signature; one is a relay that cannot hold a connection.
	DistinctHosts int
}

// RecordSessionConflict marks a relay identity as contested.
//
// It is written down rather than only logged because of who needs it and when. The operator
// who acts on a stolen credential is looking days later at a system that has since gone quiet,
// not watching a log at the moment it happened — and the relay that was displaced cannot see
// any of this from its own side.
//
// Nothing here clears the mark. Whether a contested identity has been dealt with is a judgement
// about the world outside this system, so it is left for an operator to make rather than
// erased by the next quiet hour.
func (p *Database) RecordSessionConflict(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	distinctHosts int,
) error {
	pool, err := p.Pool(organization)
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
		registrationID, organization.String(), distinctHosts)
	if err != nil {
		return fmt.Errorf("recording a session conflict: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The registration authenticated a moment ago, so its absence here means it was revoked
		// in between or the row is gone. Either way the detection has landed nowhere, and a
		// security signal that quietly writes to no rows is worse than one that never ran.
		return fmt.Errorf("recording a session conflict: registration %s not found", registrationID)
	}

	// The audit event and the current answer commit together, which is what makes the second a
	// reading of the first rather than a second opinion about it.
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

// SessionConflict reports what has been seen of a contested relay identity.
func (p *Database) SessionConflict(
	ctx context.Context, organization tenancy.Organization, registrationID uuid.UUID,
) (SessionConflict, error) {
	pool, err := p.Pool(organization)
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
		registrationID, organization.String()).Scan(&detectedAt, &hosts)
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

// ConflictWithdrawal is what withdrawing a mark actually did.
type ConflictWithdrawal int

const (
	// WithdrawalRelayUnknown means there is no such registration here.
	WithdrawalRelayUnknown ConflictWithdrawal = iota + 1
	// WithdrawalNothingMarked means the relay carried no finding. Asking again for a state that
	// already holds is not an error, and no audit event is written for a transition that did
	// not happen.
	WithdrawalNothingMarked
	// WithdrawalRecorded means a finding was withdrawn and the audit record says so.
	WithdrawalRecorded
)

// ClearSessionConflict withdraws the mark on a contested relay identity and records that it
// happened, in one transaction.
//
// Nothing clears itself. Whether a contested identity has been dealt with — a credential
// rotated, a stolen one revoked, a flapping relay fixed — is a judgement about the world
// outside this system, so it takes a deliberate act by someone who made that judgement.
//
// The act destroys the current finding, which is what makes recording it the point rather than
// a formality: without the audit record, the second occurrence would look like the first.
func (p *Database) ClearSessionConflict(
	ctx context.Context,
	principal authz.Principal,
	organization tenancy.Organization,
	registrationID uuid.UUID,
) (ConflictWithdrawal, error) {
	if !principal.MemberOf(organization) {
		return 0, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// Guarded on there being something to withdraw, so what is returned distinguishes a relay
	// that had no finding from one that has none now because this call removed it.
	tag, err := transaction.Exec(ctx, `
		UPDATE relay_registration
		   SET session_conflict_at    = NULL,
		       session_conflict_hosts = 0
		 WHERE registration_id     = $1
		   AND org_id        = $2
		   AND session_conflict_at IS NOT NULL`,
		registrationID, organization.String())
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return p.explainUnwithdrawn(ctx, organization, registrationID)
	}

	// The audit event records it at warning level in the same transaction. Withdrawing the
	// mark destroys a credential-theft finding — it is one of the highest-privilege operations
	// in the product — so an unrecordable withdrawal must not happen at all.
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

// explainUnwithdrawn reads why the guarded update matched nothing: a relay that is not here at
// all, or one that was carrying no finding to begin with.
func (p *Database) explainUnwithdrawn(
	ctx context.Context, organization tenancy.Organization, registrationID uuid.UUID,
) (ConflictWithdrawal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT true FROM relay_registration
		 WHERE registration_id = $1 AND org_id = $2`,
		registrationID, organization.String()).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return WithdrawalRelayUnknown, nil
	}
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	return WithdrawalNothingMarked, nil
}

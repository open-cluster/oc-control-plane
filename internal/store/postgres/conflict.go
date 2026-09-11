package storage

import (
	"context"
	"encoding/json"
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

	// The trail and the current answer commit together, which is what makes the second a
	// reading of the first rather than a second opinion about it.
	if err = writeEvent(ctx, transaction, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("relay session guard"),
		Action:       audit.ActionConflictDetected,
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

// ConflictEventKind is what happened to a relay identity.
type ConflictEventKind int16

const (
	// ConflictDetected is the control plane finding that an identity is being taken over.
	ConflictDetected ConflictEventKind = iota + 1
	// ConflictWithdrawn is a person saying that finding has been dealt with.
	ConflictWithdrawn
)

func (k ConflictEventKind) String() string {
	switch k {
	case ConflictDetected:
		return "detected"
	case ConflictWithdrawn:
		return "withdrawn"
	default:
		return "unrecognised"
	}
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
	// already holds is not an error, and nothing is written down for it: a trail padded with
	// acts that changed nothing is a trail nobody reads.
	WithdrawalNothingMarked
	// WithdrawalRecorded means a finding was withdrawn and the trail says so.
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
// a formality: without the trail, the second occurrence would look like the first.
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

	// The audit trail records it at warning level in the same transaction. Withdrawing the
	// mark destroys a credential-theft finding — it is one of the highest-privilege operations
	// in the product — so an unrecordable withdrawal must not happen at all.
	if err = writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionConflictCleared,
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

// ConflictEvent is one entry in a relay identity's trail.
type ConflictEvent struct {
	Kind ConflictEventKind
	At   time.Time
	// DistinctHosts is what was observed at a detection, and zero for a withdrawal.
	DistinctHosts int
	// WithdrawnFrom is where a withdrawal came from, and empty for a detection.
	WithdrawnFrom string
}

// ConflictTrail is a page of a relay identity's history.
type ConflictTrail struct {
	Events []ConflictEvent
	// Next resumes the next page, and is empty when there is none.
	Next string
}

// SessionConflictTrail returns what has happened to a relay identity, newest first.
func (p *Database) SessionConflictTrail(
	ctx context.Context,
	principal authz.Principal,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	page Page,
) (ConflictTrail, error) {
	if !principal.MemberOf(organization) {
		return ConflictTrail{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return ConflictTrail{}, err
	}
	limit := pageLimit(page.Limit)
	before, beforeID, err := decodeCursor(page.After, "-conflictAt")
	if err != nil {
		return ConflictTrail{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT event_id, action, actor_display_name, source_address, detail, occurred_at
		  FROM audit_event
		 WHERE org_id = $1
		   AND target_kind = 'relay'
		   AND target_id = $2
		   AND action IN ($6, $7)
		   AND ($4::timestamptz IS NULL
		        OR (occurred_at, event_id) < ($4::timestamptz, $5::uuid))
		 ORDER BY occurred_at DESC, event_id DESC
		 LIMIT $3`,
		organization.String(), registrationID.String(), limit+1, before, beforeID,
		audit.ActionConflictDetected, audit.ActionConflictCleared)
	if err != nil {
		return ConflictTrail{}, fmt.Errorf("reading a session conflict trail: %w", err)
	}
	defer rows.Close()

	trail := ConflictTrail{Events: make([]ConflictEvent, 0, limit)}
	var lastAt time.Time
	var lastID uuid.UUID
	for rows.Next() {
		identifier, event, scanErr := scanConflictEvent(rows)
		if scanErr != nil {
			return ConflictTrail{}, scanErr
		}
		if len(trail.Events) == limit {
			// The cursor is the last event RETURNED, not the extra one read to detect it. The
			// next page resumes strictly after what the caller has seen, so the row that proved
			// there was more is the first row they get rather than one they never see.
			trail.Next = encodeCursor("-conflictAt", lastAt, lastID)
			break
		}
		lastID, lastAt = identifier, event.At
		trail.Events = append(trail.Events, event)
	}
	if err = rows.Err(); err != nil {
		return ConflictTrail{}, fmt.Errorf("reading a session conflict trail: %w", err)
	}
	return trail, nil
}

func scanConflictEvent(rows pgx.Rows) (uuid.UUID, ConflictEvent, error) {
	var (
		identifier uuid.UUID
		event      ConflictEvent
		action     audit.Action
		actor      string
		address    string
		detail     []byte
	)
	if err := rows.Scan(&identifier, &action, &actor, &address, &detail, &event.At); err != nil {
		return uuid.Nil, ConflictEvent{}, fmt.Errorf("reading a session conflict event: %w", err)
	}
	switch action {
	case audit.ActionConflictDetected:
		event.Kind = ConflictDetected
		var context struct {
			DistinctHosts int `json:"distinctHosts"`
		}
		if err := json.Unmarshal(detail, &context); err != nil {
			return uuid.Nil, ConflictEvent{}, fmt.Errorf("reading session conflict detail: %w", err)
		}
		event.DistinctHosts = context.DistinctHosts
	case audit.ActionConflictCleared:
		event.Kind = ConflictWithdrawn
		event.WithdrawnFrom = actor
		if address != "" {
			event.WithdrawnFrom += " (" + address + ")"
		}
	default:
		return uuid.Nil, ConflictEvent{}, fmt.Errorf("reading a session conflict event: unknown action %q", action)
	}
	return identifier, event, nil
}

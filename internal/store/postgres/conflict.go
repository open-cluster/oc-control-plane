package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
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
	if err = appendConflictEvent(ctx, transaction, conflictEvent{
		organization:   organization,
		registrationID: registrationID,
		kind:           ConflictDetected,
		distinctHosts:  distinctHosts,
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

// conflictEvent is one entry on its way into the trail.
type conflictEvent struct {
	organization   tenancy.Organization
	registrationID uuid.UUID
	kind           ConflictEventKind
	distinctHosts  int
	// withdrawnFrom is the address a withdrawal came from, and empty for a detection.
	withdrawnFrom string
}

func appendConflictEvent(ctx context.Context, transaction pgx.Tx, event conflictEvent) error {
	var actor *string
	if event.withdrawnFrom != "" {
		actor = &event.withdrawnFrom
	}
	_, err := transaction.Exec(ctx, `
		INSERT INTO relay_session_conflict_event
			(org_id, registration_id, kind, distinct_hosts, withdrawn_from)
		VALUES ($1, $2, $3, $4, $5)`,
		event.organization.String(), event.registrationID,
		int16(event.kind), event.distinctHosts, actor)
	if err != nil {
		return fmt.Errorf("appending a session conflict event: %w", err)
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

	// The conflict trail records WHAT happened to the relay identity. It now records who: the
	// principal's name goes in the trail's own withdrawnFrom field rather than an address,
	// which is the limit this surface used to state out loud and no longer has.
	if err = appendConflictEvent(ctx, transaction, conflictEvent{
		organization:   organization,
		registrationID: registrationID,
		kind:           ConflictWithdrawn,
		withdrawnFrom:  principal.DisplayName() + " (" + principal.SourceAddress() + ")",
	}); err != nil {
		return 0, err
	}
	// And the audit trail records it at warning level, in the same transaction. Withdrawing the
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
	before, err := decodeEventCursor(page.After)
	if err != nil {
		return ConflictTrail{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT event_id, kind, distinct_hosts, withdrawn_from, at
		  FROM relay_session_conflict_event
		 WHERE org_id    = $1
		   AND registration_id = $2
		   AND ($4::bigint IS NULL OR event_id < $4::bigint)
		 ORDER BY event_id DESC
		 LIMIT $3`,
		organization.String(), registrationID, limit+1, before)
	if err != nil {
		return ConflictTrail{}, fmt.Errorf("reading a session conflict trail: %w", err)
	}
	defer rows.Close()

	trail := ConflictTrail{Events: make([]ConflictEvent, 0, limit)}
	var last int64
	for rows.Next() {
		identifier, event, scanErr := scanConflictEvent(rows)
		if scanErr != nil {
			return ConflictTrail{}, scanErr
		}
		if len(trail.Events) == limit {
			// The cursor is the last event RETURNED, not the extra one read to detect it. The
			// next page resumes strictly after what the caller has seen, so the row that proved
			// there was more is the first row they get rather than one they never see.
			trail.Next = encodeEventCursor(last)
			break
		}
		last = identifier
		trail.Events = append(trail.Events, event)
	}
	if err = rows.Err(); err != nil {
		return ConflictTrail{}, fmt.Errorf("reading a session conflict trail: %w", err)
	}
	return trail, nil
}

func scanConflictEvent(rows pgx.Rows) (int64, ConflictEvent, error) {
	var (
		identifier    int64
		event         ConflictEvent
		withdrawnFrom *string
	)
	if err := rows.Scan(&identifier, &event.Kind, &event.DistinctHosts,
		&withdrawnFrom, &event.At); err != nil {
		return 0, ConflictEvent{}, fmt.Errorf("reading a session conflict event: %w", err)
	}
	if withdrawnFrom != nil {
		event.WithdrawnFrom = *withdrawnFrom
	}
	return identifier, event, nil
}

// encodeEventCursor renders a position in the trail. It is opaque so that a caller cannot read
// a row count out of it: the identity is assigned across every organization in the database,
// and its value is nobody's business but this table's.
func encodeEventCursor(identifier int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(identifier, 10)))
}

func decodeEventCursor(cursor string) (*int64, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrBadCursor
	}
	identifier, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil, ErrBadCursor
	}
	return &identifier, nil
}

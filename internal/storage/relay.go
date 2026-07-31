package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ErrEnrolmentRefused reports that a bootstrap token did not entitle its presenter to an
// identity. Every reason produces this one error, because telling an unknown token from a
// spent one is exactly what lets an attacker probe for valid tokens. The distinction is
// carried alongside it for the audit trail and must not reach the presenter.
var ErrEnrolmentRefused = errors.New("relay enrolment refused")

// EnrolmentRefusal is why an enrolment was refused. It exists for the server-side audit
// trail; it is never rendered into a response.
type EnrolmentRefusal int

const (
	// RefusalNone is the zero value, used when no refusal occurred.
	RefusalNone EnrolmentRefusal = iota
	RefusalTokenUnknown
	RefusalTokenExpired
	RefusalTokenAlreadyConsumed
	RefusalTokenRevoked
	RefusalOrganizationMismatch
)

func (r EnrolmentRefusal) String() string {
	switch r {
	case RefusalNone:
		return "none"
	case RefusalTokenUnknown:
		return "token unknown"
	case RefusalTokenExpired:
		return "token expired"
	case RefusalTokenAlreadyConsumed:
		return "token already consumed"
	case RefusalTokenRevoked:
		return "token revoked"
	case RefusalOrganizationMismatch:
		return "token issued for another organization"
	default:
		return "unrecognised"
	}
}

// RelayEnrolment is everything a relay presents at registration, already reduced to what is
// durable. The bootstrap token and the credential appear only as digests: this type crosses
// no boundary carrying a secret in a form that could be stored or logged by accident.
type RelayEnrolment struct {
	TokenDigest        []byte
	CredentialDigest   []byte
	ClusterFingerprint string
	RelayVersion       string
	Capabilities       []byte
}

// EnrolRelay spends the bootstrap token and records the identity it mints, in one
// transaction. Both happen or neither does: a token consumed without an identity issued
// would strand an operator with an installation that can never register, and an identity
// without the token spent would let one token enrol a second relay.
//
// Concurrency is resolved by the database rather than by application locking. Two
// simultaneous presentations of one token serialise on its row, so exactly one observes it
// unspent and the other is refused. An application-level guard would be a second source of
// truth for something the row already decides.
func (p *Placements) EnrolRelay(
	ctx context.Context,
	organization tenancy.Organization,
	enrolment RelayEnrolment,
) (uuid.UUID, EnrolmentRefusal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return uuid.Nil, RefusalNone, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, RefusalNone, fmt.Errorf("beginning enrolment: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	spent, err := spendBootstrapToken(ctx, transaction, organization, enrolment.TokenDigest)
	if err != nil {
		return uuid.Nil, RefusalNone, err
	}
	if !spent {
		// The token was not spendable. Read why, for the audit trail only, from inside the
		// same transaction so the explanation describes the state the guard actually saw.
		refusal, reasonErr := explainUnspendableToken(ctx, transaction, organization, enrolment.TokenDigest)
		if reasonErr != nil {
			return uuid.Nil, RefusalNone, reasonErr
		}
		return uuid.Nil, refusal, ErrEnrolmentRefused
	}

	registration := relayRegistration{
		id:           uuid.New(),
		organization: organization,
		enrolment:    enrolment,
	}
	if err = insertRegistration(ctx, transaction, registration); err != nil {
		return uuid.Nil, RefusalNone, err
	}
	registrationID := registration.id
	if err = transaction.Commit(ctx); err != nil {
		return uuid.Nil, RefusalNone, fmt.Errorf("committing enrolment: %w", err)
	}
	return registrationID, RefusalNone, nil
}

// spendBootstrapToken marks the token consumed, reporting whether this call was the one that
// spent it. Every condition is in the WHERE clause, so the decision is the database's and
// cannot be raced between a read and a write.
func spendBootstrapToken(
	ctx context.Context,
	transaction pgx.Tx,
	organization tenancy.Organization,
	tokenDigest []byte,
) (bool, error) {
	tag, err := transaction.Exec(ctx, `
		UPDATE relay_bootstrap_token
		   SET consumed_at = now()
		 WHERE token_digest = $1
		   AND organization = $2
		   AND consumed_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > now()`,
		tokenDigest, organization.String())
	if err != nil {
		return false, fmt.Errorf("consuming bootstrap token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// explainUnspendableToken reports why the guarded update matched nothing. Its result reaches
// the audit trail and never the caller of Register.
func explainUnspendableToken(ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, tokenDigest []byte) (EnrolmentRefusal, error) {
	var (
		tokenOrganization string
		consumed          bool
		revoked           bool
		expired           bool
	)
	err := transaction.QueryRow(ctx, `
		SELECT organization,
		       consumed_at IS NOT NULL,
		       revoked_at IS NOT NULL,
		       expires_at <= now()
		  FROM relay_bootstrap_token
		 WHERE token_digest = $1`, tokenDigest).
		Scan(&tokenOrganization, &consumed, &revoked, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RefusalTokenUnknown, nil
	case err != nil:
		return RefusalNone, fmt.Errorf("auditing refused enrolment: %w", err)
	case tokenOrganization != organization.String():
		return RefusalOrganizationMismatch, nil
	case revoked:
		return RefusalTokenRevoked, nil
	case consumed:
		return RefusalTokenAlreadyConsumed, nil
	case expired:
		return RefusalTokenExpired, nil
	default:
		// The row is spendable now, so it was spent and rolled back between the update and
		// this read. Reporting it as already consumed is the honest description.
		return RefusalTokenAlreadyConsumed, nil
	}
}

// relayRegistration is the row about to be written, bundled so the insert takes what it
// needs as one value rather than as a widening parameter list.
type relayRegistration struct {
	id           uuid.UUID
	organization tenancy.Organization
	enrolment    RelayEnrolment
}

func insertRegistration(ctx context.Context, transaction pgx.Tx, registration relayRegistration) error {
	enrolment := registration.enrolment
	_, err := transaction.Exec(ctx, `
		INSERT INTO relay_registration
			(registration_id, organization, credential_digest,
			 cluster_fingerprint, relay_version, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		registration.id, registration.organization.String(), enrolment.CredentialDigest,
		enrolment.ClusterFingerprint, enrolment.RelayVersion, enrolment.Capabilities)
	if err != nil {
		return fmt.Errorf("recording relay registration: %w", err)
	}
	return nil
}

// IssueBootstrapToken records a single-use enrolment token for an organization. Only the
// digest is stored, so this is the one moment the token exists here; the caller shows it to
// the operator once and keeps no copy either.
func (p *Placements) IssueBootstrapToken(
	ctx context.Context,
	organization tenancy.Organization,
	tokenDigest []byte,
	expiresAt time.Time,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO relay_bootstrap_token (token_digest, organization, expires_at)
		VALUES ($1, $2, $3)`,
		tokenDigest, organization.String(), expiresAt)
	if err != nil {
		return fmt.Errorf("issuing bootstrap token: %w", err)
	}
	return nil
}

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
func (p *Placements) RecordSessionConflict(
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
		   AND organization    = $2`,
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
			(organization, registration_id, kind, distinct_hosts, withdrawn_from)
		VALUES ($1, $2, $3, $4, $5)`,
		event.organization.String(), event.registrationID,
		int16(event.kind), event.distinctHosts, actor)
	if err != nil {
		return fmt.Errorf("appending a session conflict event: %w", err)
	}
	return nil
}

// SessionConflict reports what has been seen of a contested relay identity.
func (p *Placements) SessionConflict(
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
		 WHERE registration_id = $1 AND organization = $2`,
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
func (p *Placements) ClearSessionConflict(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	withdrawnFrom string,
) (ConflictWithdrawal, error) {
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
		   AND organization        = $2
		   AND session_conflict_at IS NOT NULL`,
		registrationID, organization.String())
	if err != nil {
		return 0, fmt.Errorf("withdrawing a session conflict: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return p.explainUnwithdrawn(ctx, organization, registrationID)
	}

	if err = appendConflictEvent(ctx, transaction, conflictEvent{
		organization:   organization,
		registrationID: registrationID,
		kind:           ConflictWithdrawn,
		withdrawnFrom:  withdrawnFrom,
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
func (p *Placements) explainUnwithdrawn(
	ctx context.Context, organization tenancy.Organization, registrationID uuid.UUID,
) (ConflictWithdrawal, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT true FROM relay_registration
		 WHERE registration_id = $1 AND organization = $2`,
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
func (p *Placements) SessionConflictTrail(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	page Page,
) (ConflictTrail, error) {
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
		 WHERE organization    = $1
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
// a row count out of it: the identity is assigned across every organization on the placement,
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

// RelaySummary is a relay identity as an operator needs to see it.
//
// It carries no credential digest. Nothing an operator does with this list needs one, and a
// read model that carries a secret is that secret in one more place — in memory, in a JSON
// response, and in whatever logs or exports the response passes through afterwards.
type RelaySummary struct {
	RegistrationID     uuid.UUID
	ClusterFingerprint string
	RelayVersion       string
	RegisteredAt       time.Time
	// RevokedAt is zero for a live registration.
	RevokedAt time.Time
	Conflict  SessionConflict
}

// Page is which slice of a listing to return. It is not relay-specific: every paged read on
// this surface takes the same shape, and naming it for the first caller would have made
// "Relay" mean something it does not the moment the second one arrived.
type Page struct {
	// Limit is how many to return. Zero means the default rather than none: a caller that names
	// no size wants the list, and answering with one row would hide the very findings this is
	// read for.
	Limit int
	// After resumes from a previous page's Next. An empty value starts at the beginning.
	After string
}

// RelayRoster is a page of an organization's relay identities.
type RelayRoster struct {
	Relays []RelaySummary
	// Next resumes the next page, and is empty when there is none. Its presence is also how a
	// caller knows relays were left out: a truncated list that looks complete is how an
	// operator concludes a relay is gone.
	Next string
}

// Bounds on a page. An operator asking for everything still gets a bounded answer,
// because an unbounded list is a query whose cost belongs to whoever calls it most — and the
// cursor is what makes that bound a page rather than a ceiling on what can ever be seen.
const (
	defaultRosterPage = 50
	maxRosterPage     = 200
)

// ErrBadCursor reports a resume point that did not come from a previous page.
var ErrBadCursor = errors.New("cursor is not a page position")

// ListRelays returns an organization's relay identities, newest first.
func (p *Placements) ListRelays(
	ctx context.Context, organization tenancy.Organization, page Page,
) (RelayRoster, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return RelayRoster{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return RelayRoster{}, err
	}

	// The position is (created_at, registration_id) rather than an offset, so a relay enrolled
	// while an operator is paging cannot shift the rows underneath them and hide one.
	//
	// One more row than asked for is read, so whether anything was left out is read from the
	// data rather than guessed at by comparing a count to a limit.
	rows, err := pool.Query(ctx, `
		SELECT registration_id, cluster_fingerprint, relay_version, created_at,
		       revoked_at, session_conflict_at, session_conflict_hosts
		  FROM relay_registration
		 WHERE organization = $1
		   AND ($3::timestamptz IS NULL
		        OR (created_at, registration_id) < ($3::timestamptz, $4::uuid))
		 ORDER BY created_at DESC, registration_id DESC
		 LIMIT $2`,
		organization.String(), limit+1, after, afterID)
	if err != nil {
		return RelayRoster{}, fmt.Errorf("listing relays: %w", err)
	}
	defer rows.Close()

	roster := RelayRoster{Relays: make([]RelaySummary, 0, limit)}
	for rows.Next() {
		summary, scanErr := scanRelaySummary(rows)
		if scanErr != nil {
			return RelayRoster{}, scanErr
		}
		if len(roster.Relays) == limit {
			last := roster.Relays[limit-1]
			roster.Next = encodeCursor(last.RegisteredAt, last.RegistrationID)
			break
		}
		roster.Relays = append(roster.Relays, summary)
	}
	if err = rows.Err(); err != nil {
		return RelayRoster{}, fmt.Errorf("listing relays: %w", err)
	}
	return roster, nil
}

// pageLimit resolves how many rows to return. A caller that named nothing gets the default,
// not the minimum.
func pageLimit(asked int) int {
	if asked <= 0 {
		return defaultRosterPage
	}
	return min(asked, maxRosterPage)
}

// encodeCursor renders a page position. It is opaque on purpose: a caller that took it apart
// would be depending on the ordering rather than on the cursor, and the ordering is ours.
func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(at.UnixNano(), 10) + ":" + id.String()))
}

// decodeCursor reads a page position back. An empty cursor is the start of the list, which is
// not an error; anything unreadable is, because silently starting over would show an operator
// the first page again and let them believe they had seen the last.
func decodeCursor(cursor string) (*time.Time, *uuid.UUID, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	nanos, identifier, found := strings.Cut(string(raw), ":")
	if !found {
		return nil, nil, ErrBadCursor
	}
	unixNano, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	id, err := uuid.Parse(identifier)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	at := time.Unix(0, unixNano)
	return &at, &id, nil
}

func scanRelaySummary(rows pgx.Rows) (RelaySummary, error) {
	var (
		summary    RelaySummary
		revokedAt  *time.Time
		conflictAt *time.Time
		hosts      int
	)
	if err := rows.Scan(&summary.RegistrationID, &summary.ClusterFingerprint,
		&summary.RelayVersion, &summary.RegisteredAt, &revokedAt, &conflictAt, &hosts); err != nil {
		return RelaySummary{}, fmt.Errorf("reading a relay: %w", err)
	}
	if revokedAt != nil {
		summary.RevokedAt = *revokedAt
	}
	if conflictAt != nil {
		summary.Conflict = SessionConflict{DetectedAt: *conflictAt, DistinctHosts: hosts}
	}
	return summary, nil
}

// VerifyRelayCredential reports whether a credential digest matches a live registration.
// It fails closed: a revoked registration authenticates nothing, and an unknown one is
// indistinguishable from a wrong credential.
func (p *Placements) VerifyRelayCredential(
	ctx context.Context,
	organization tenancy.Organization,
	registrationID uuid.UUID,
	credentialDigest []byte,
) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	var matches bool
	err = pool.QueryRow(ctx, `
		SELECT credential_digest = $3
		  FROM relay_registration
		 WHERE registration_id = $1
		   AND organization    = $2
		   AND revoked_at IS NULL`,
		registrationID, organization.String(), credentialDigest).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verifying relay credential: %w", err)
	}
	return matches, nil
}

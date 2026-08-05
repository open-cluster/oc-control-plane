package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The fleet as a whole, and the durable presence that lets it be answered at all.
//
// A hundred relays is a hundred rows, and a hundred rows is not an assessment. What a platform
// engineer needs before reading any of them is how many there are, how many are connected, how
// many are behind, and how much work is in flight — which is one query rather than a hundred,
// and which nothing in this product could answer before.

// Fleet is an organization's relays counted rather than listed.
type Fleet struct {
	Total        int
	Connected    int
	Disconnected int
	Revoked      int
	// Outdated is how many are running a version below the floor this build states. It is a
	// count of what needs upgrading, which is the question behind the version column.
	Outdated int
	// Degraded is how many carry an unresolved session conflict: something is holding that
	// relay's credential alongside it. It is counted separately because it is the one state
	// where a row looks healthy and is not.
	Degraded int
	// ActiveRequests is how much work the fleet is holding right now — jobs leased and not yet
	// finished. It is the fleet's load, and it is read from the job table rather than from any
	// relay's own account of itself.
	ActiveRequests int
	// LivenessWindow is how recently a relay must have been heard from to be counted connected.
	// It is reported so a number nobody can interpret does not have to be.
	LivenessWindow time.Duration
	// MinimumVersion is the floor Outdated was counted against. Empty means this build states
	// no floor, in which case Outdated is zero because nothing was compared rather than because
	// everything is current — and the two are different facts.
	MinimumVersion string
}

// FleetSummary counts an organization's relays.
//
// Every number comes from ONE query, so the counts cannot disagree with each other the way
// separate reads at separate moments would — a summary saying eleven connected out of ten is
// worse than no summary.
func (p *Placements) FleetSummary(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	liveness time.Duration, minimumVersion string,
) (Fleet, error) {
	if !principal.MemberOf(organization) {
		return Fleet{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return Fleet{}, err
	}

	fleet := Fleet{LivenessWindow: liveness, MinimumVersion: minimumVersion}
	err = pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE `+relayConnectedExpression+`),
		       count(*) FILTER (WHERE NOT `+relayConnectedExpression+`
		                          AND registration.revoked_at IS NULL),
		       count(*) FILTER (WHERE registration.revoked_at IS NOT NULL),
		       -- Outdated is counted only where a floor was stated. Comparing against an empty
		       -- string would count every relay as outdated, which is a number that would send
		       -- somebody to upgrade a fleet that is fine.
		       count(*) FILTER (WHERE $3::text <> ''
		                          AND registration.revoked_at IS NULL
		                          AND registration.relay_version < $3::text),
		       count(*) FILTER (WHERE registration.session_conflict_at IS NOT NULL),
		       (SELECT count(*) FROM relay_job
		         WHERE organization = $1 AND status = 1)
		  FROM relay_registration registration
		 WHERE registration.organization = $1`,
		organization.String(), liveness, minimumVersion).
		Scan(&fleet.Total, &fleet.Connected, &fleet.Disconnected, &fleet.Revoked,
			&fleet.Outdated, &fleet.Degraded, &fleet.ActiveRequests)
	if err != nil {
		return Fleet{}, fmt.Errorf("summarising a relay fleet: %w", err)
	}
	return fleet, nil
}

// RelayConnections lists the Connections a Relay serves, so an operator knows what disabling it
// costs before they disable it.
//
// It goes through the same query every other Connection listing does, rather than carrying a
// second one of its own: a Relay's Connections are an Organization's Connections narrowed by the
// Relay bound to them, and a second query would be a second place the columns, the paging and
// the ordering could drift.
func (p *Placements) RelayConnections(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	registration uuid.UUID, query ConnectionQuery,
) (ConnectionList, error) {
	query.Relay = registration
	return p.QueryConnections(ctx, principal, organization, query)
}

// IssueOperatorBootstrapToken records a single-use enrolment token an operator asked for, and
// the audit event saying who asked.
//
// It is a second entry point rather than a parameter on IssueBootstrapToken because the two have
// genuinely different contracts: the existing one is used by the composition root at startup and
// has no principal to attribute, and this one is a person handing themselves a credential that
// enrols a relay into their tenant. The second must be on the record; the first has nobody to
// put on it.
//
// Only the digest is stored, so this is the one moment the token exists here. The caller shows
// it once and keeps no copy either.
func (p *Placements) IssueOperatorBootstrapToken(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	tokenDigest []byte, expiresAt time.Time,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionRelayBootstrapIssued,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			if _, err := transaction.Exec(ctx, `
				INSERT INTO relay_bootstrap_token (token_digest, organization, expires_at)
				VALUES ($1, $2, $3)`,
				tokenDigest, organization.String(), expiresAt); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("issuing a bootstrap token: %w", err)
			}
			// The token is nowhere in the detail and could not be: audit.Detail drops anything
			// named like a credential on the way in. What the record needs is that one was
			// issued, by whom, and how long it stays spendable.
			return struct{}{},
				audit.Target{Kind: audit.TargetRelay, ID: "bootstrap-token"},
				audit.Detail{
					"expiresAt": expiresAt.UTC().Format(time.RFC3339),
					"effect": "a single-use relay enrolment token was issued and shown once; " +
						"it cannot be read back",
				}, nil
		})
	return err
}

// ---------------------------------------------------------------------------------------
// Durable presence
// ---------------------------------------------------------------------------------------

// RelaySessionOpened records that this process is now holding a relay's session.
//
// The session identifier is written with it and every later write is guarded on it, so a
// session that has already been displaced cannot clear its successor's presence on the way out.
// That is the same rule the in-memory registry follows, made durable — and it has to be durable,
// because the registry is per process and a fleet summary built from one process's view would
// report a fraction of the fleet as the fleet.
func (p *Placements) RelaySessionOpened(
	ctx context.Context, organization tenancy.Organization, registration, session uuid.UUID,
	peer string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		UPDATE relay_registration
		   SET session_id         = $3,
		       session_started_at = now(),
		       session_ended_at   = NULL,
		       last_seen_at       = now(),
		       session_peer       = $4
		 WHERE organization = $1 AND registration_id = $2`,
		organization.String(), registration, session, boundedPeer(peer)); err != nil {
		return fmt.Errorf("recording a relay session: %w", err)
	}
	return nil
}

// RelaySessionHeard records that the relay is still alive.
//
// Guarded on the session identifier: a heartbeat from a session that has been displaced must not
// refresh the presence of the registration its successor now holds, or a dying session would
// keep a relay looking connected through whichever process was slowest to notice.
func (p *Placements) RelaySessionHeard(
	ctx context.Context, organization tenancy.Organization, registration, session uuid.UUID,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		UPDATE relay_registration
		   SET last_seen_at = now()
		 WHERE organization = $1 AND registration_id = $2 AND session_id = $3`,
		organization.String(), registration, session); err != nil {
		return fmt.Errorf("recording that a relay was heard: %w", err)
	}
	return nil
}

// RelaySessionClosed records that this session has ended.
//
// Guarded the same way, and for the same reason: a replaced session must not evict its
// successor. A session that dies without reaching this leaves last_seen_at behind, and the
// liveness window is what bounds how long that looks connected — which is the same allowance
// the in-memory watch uses, so the durable answer and the live one agree.
func (p *Placements) RelaySessionClosed(
	ctx context.Context, organization tenancy.Organization, registration, session uuid.UUID,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		UPDATE relay_registration
		   SET session_ended_at = now()
		 WHERE organization = $1 AND registration_id = $2 AND session_id = $3
		   AND session_ended_at IS NULL`,
		organization.String(), registration, session); err != nil {
		return fmt.Errorf("recording the end of a relay session: %w", err)
	}
	return nil
}

// boundedPeer keeps the column's bound from being something a caller decides. The address comes
// from the transport rather than from a message, so this is a belt on a brace.
func boundedPeer(peer string) string {
	const most = 256
	if len(peer) > most {
		return peer[:most]
	}
	return peer
}

// RelayFailure is one execution this Relay did not complete.
//
// It carries no failure MESSAGE, because relay_job records none: a job is succeeded, failed or
// cancelled, and the reason a relay gave is not a column. What this can say is which capability
// failed, against which Connection, and when — which is what separates "this relay is unwell"
// from "this one capability is unwell on this relay", and that is the distinction somebody
// diagnosing an intermittent Relay is actually trying to make.
type RelayFailure struct {
	JobID             uuid.UUID
	CapabilityID      string
	CapabilityVersion int
	Connection        uuid.UUID
	// Cancelled separates work that was called off from work that failed. Both are executions
	// that produced nothing, and only one of them is the Relay's fault.
	Cancelled bool
	At        time.Time
}

// RelayFailureList is a page of a Relay's recent failures, newest first.
type RelayFailureList struct {
	Failures []RelayFailure
	Next     string
}

// RelayFailures reads what a Relay has recently failed to complete, so an intermittent one can be
// diagnosed from the record rather than from whoever happened to be watching.
func (p *Placements) RelayFailures(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	registration uuid.UUID, page Page,
) (RelayFailureList, error) {
	if !principal.MemberOf(organization) {
		return RelayFailureList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return RelayFailureList{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return RelayFailureList{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT job_id, capability_id, capability_version, connection_id, status, terminal_at
		  FROM relay_job
		 WHERE organization = $1 AND registration_id = $2 AND status IN (3, 4)
		   AND ($4::timestamptz IS NULL
		        OR (terminal_at, job_id) < ($4::timestamptz, $5::uuid))
		 ORDER BY terminal_at DESC, job_id DESC
		 LIMIT $3`,
		organization.String(), registration, limit+1, after, afterID)
	if err != nil {
		return RelayFailureList{}, fmt.Errorf("reading a relay's failures: %w", err)
	}
	defer rows.Close()

	list := RelayFailureList{Failures: make([]RelayFailure, 0, limit)}
	for rows.Next() {
		var (
			failure RelayFailure
			status  int16
		)
		if err = rows.Scan(&failure.JobID, &failure.CapabilityID, &failure.CapabilityVersion,
			&failure.Connection, &status, &failure.At); err != nil {
			return RelayFailureList{}, fmt.Errorf("reading a relay failure: %w", err)
		}
		if len(list.Failures) == limit {
			last := list.Failures[limit-1]
			list.Next = encodeCursor(last.At, last.JobID)
			break
		}
		failure.Cancelled = JobStatus(status) == JobCancelled
		list.Failures = append(list.Failures, failure)
	}
	if err = rows.Err(); err != nil {
		return RelayFailureList{}, fmt.Errorf("reading a relay's failures: %w", err)
	}
	return list, nil
}

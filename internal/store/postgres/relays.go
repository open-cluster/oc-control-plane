package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type RelayCounts struct {
	Total          int
	Connected      int
	Disconnected   int
	Revoked        int
	Degraded       int
	ActiveRequests int
	LivenessWindow time.Duration
}

func (p *Database) CountRelays(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	liveness time.Duration,
) (RelayCounts, error) {
	if principal.Organization() != organization {
		return RelayCounts{}, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return RelayCounts{}, err
	}

	summary := RelayCounts{LivenessWindow: liveness}
	err = pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE `+relayConnectedExpression+`),
		       count(*) FILTER (WHERE NOT `+relayConnectedExpression+`
		                          AND registration.revoked_at IS NULL),
		       count(*) FILTER (WHERE registration.revoked_at IS NOT NULL),
		       count(*) FILTER (WHERE registration.session_conflict_at IS NOT NULL),
		       (SELECT count(*) FROM relay_job
		         WHERE org_id = $1 AND status = 1)
		  FROM relay_registration registration
		 WHERE registration.org_id = $1`,
		organization, liveness).
		Scan(&summary.Total, &summary.Connected, &summary.Disconnected, &summary.Revoked,
			&summary.Degraded, &summary.ActiveRequests)
	if err != nil {
		return RelayCounts{}, fmt.Errorf("summarising relays: %w", err)
	}
	return summary, nil
}

func (p *Database) IssueOperatorBootstrapToken(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	tokenDigest []byte, expiresAt time.Time,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionRelayBootstrapIssued,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			if _, err := transaction.Exec(ctx, `
				INSERT INTO relay_bootstrap_token (bootstrap_digest, org_id, expires_at)
				VALUES ($1, $2, $3)`,
				tokenDigest, organization, expiresAt); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("issuing a bootstrap token: %w", err)
			}
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

func (p *Database) RelaySessionOpened(
	ctx context.Context, organization uuid.UUID, registration, session uuid.UUID,
	peer string,
) error {
	pool, err := p.poolForOrganization(organization)
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
		 WHERE org_id = $1 AND registration_id = $2`,
		organization, registration, session, boundedPeer(peer)); err != nil {
		return fmt.Errorf("recording a relay session: %w", err)
	}
	return nil
}

func (p *Database) RelaySessionHeard(
	ctx context.Context, organization uuid.UUID, registration, session uuid.UUID,
) error {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		UPDATE relay_registration
		   SET last_seen_at = now()
		 WHERE org_id = $1 AND registration_id = $2 AND session_id = $3`,
		organization, registration, session); err != nil {
		return fmt.Errorf("recording that a relay was heard: %w", err)
	}
	return nil
}

func (p *Database) RelaySessionClosed(
	ctx context.Context, organization uuid.UUID, registration, session uuid.UUID,
) error {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `
		UPDATE relay_registration
		   SET session_ended_at = now()
		 WHERE org_id = $1 AND registration_id = $2 AND session_id = $3
		   AND session_ended_at IS NULL`,
		organization, registration, session); err != nil {
		return fmt.Errorf("recording the end of a relay session: %w", err)
	}
	return nil
}

func boundedPeer(peer string) string {
	const most = 256
	if len(peer) > most {
		return peer[:most]
	}
	return peer
}

type RelayFailure struct {
	JobID             uuid.UUID
	CapabilityID      string
	CapabilityVersion int
	Integration       uuid.UUID
	Cancelled         bool
	At                time.Time
}

type RelayFailureList struct {
	Failures []RelayFailure
	Next     string
}

func (p *Database) RelayFailures(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	registration uuid.UUID, page Page,
) (RelayFailureList, error) {
	if principal.Organization() != organization {
		return RelayFailureList{}, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return RelayFailureList{}, err
	}
	limit := pageLimit(page.Limit)
	after, afterID, err := decodeCursor(page.After, "-at")
	if err != nil {
		return RelayFailureList{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT job_id, capability_id, capability_version, integration_id, status, terminal_at
		  FROM relay_job
		 WHERE org_id = $1 AND registration_id = $2 AND status IN (3, 4)
		   AND ($4::timestamptz IS NULL
		        OR (terminal_at, job_id) < ($4::timestamptz, $5::uuid))
		 ORDER BY terminal_at DESC, job_id DESC
		 LIMIT $3`,
		organization, registration, limit+1, after, afterID)
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
			&failure.Integration, &status, &failure.At); err != nil {
			return RelayFailureList{}, fmt.Errorf("reading a relay failure: %w", err)
		}
		if len(list.Failures) == limit {
			last := list.Failures[limit-1]
			list.Next = encodeCursor("-at", last.At, last.JobID)
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

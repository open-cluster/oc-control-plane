package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

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

// RelayRoster is a page of an organization's relay identities.
type RelayRoster struct {
	Relays []RelaySummary
	// Next resumes the next page, and is empty when there is none. Its presence is also how a
	// caller knows relays were left out: a truncated list that looks complete is how an
	// operator concludes a relay is gone.
	Next string
}

// ListRelays returns an organization's relay identities, newest first.
//
// It takes the principal for the same reason every operator-facing read does: the
// authorization middleware has already decided, and this is the layer that cannot be reached
// around. A principal with no membership is refused here even if it arrived from a path
// nobody routed through the middleware.
func (p *Placements) ListRelays(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization, page Page,
) (RelayRoster, error) {
	if !principal.MemberOf(organization) {
		return RelayRoster{}, ErrNotAMember
	}
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

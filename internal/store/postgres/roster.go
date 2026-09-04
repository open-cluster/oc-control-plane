package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// RelaySummary excludes credential material.
type RelaySummary struct {
	RegistrationID     uuid.UUID
	ClusterFingerprint string
	RelayVersion       string
	ProtocolVersion    uint32
	RegisteredAt       time.Time
	RevokedAt          time.Time
	Conflict           SessionConflict
	Connected          bool
	LastSeenAt         time.Time
	SessionPeer        string
	Capabilities       []string
}

type RelayRoster struct {
	Relays []RelaySummary
	Next   string
	Total  *int
}

type RelayQuery struct {
	Page           Page
	Search         string
	State          string
	Version        string
	Capability     string
	SortField      string
	Descending     bool
	LivenessWindow time.Duration
}

// Public sort names map only to fixed SQL expressions.
var relayOrderings = map[string]struct {
	column string
	cast   string
}{
	"registeredAt": {"registration.created_at", "timestamptz"},
	"lastSeenAt":   {"registration.last_seen_at", "timestamptz"},
	"version":      {"registration.relay_version", "text"},
	"fingerprint":  {"registration.cluster_fingerprint", "text"},
}

func (p *Database) ListRelays(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	query RelayQuery,
) (RelayRoster, error) {
	if !principal.MemberOf(organization) {
		return RelayRoster{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return RelayRoster{}, err
	}
	ordering, known := relayOrderings[query.SortField]
	if query.SortField == "" {
		query.SortField = "registeredAt"
		ordering = relayOrderings["registeredAt"]
		query.Descending = true
	} else if !known {
		return RelayRoster{}, fmt.Errorf("relay listing cannot order by %q", query.SortField)
	}
	if query.SortField == "lastSeenAt" {
		nullValue := "'infinity'::timestamptz"
		if query.Descending {
			nullValue = "'-infinity'::timestamptz"
		}
		ordering.column = "coalesce(registration.last_seen_at, " + nullValue + ")"
	}
	limit := pageLimit(query.Page.Limit)
	scope := sortScope(query.SortField, query.Descending)
	cursorValue, cursorID, err := decodeSortCursor(query.Page.After, scope)
	if err != nil {
		return RelayRoster{}, err
	}

	arguments := []any{organization.String(), query.LivenessWindow}
	where := []string{"registration.org_id = $1"}
	add := func(clause string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf(clause, len(arguments)))
	}

	if query.Search != "" {
		add(`(registration.cluster_fingerprint ILIKE '%%' || $%[1]d || '%%'
		      OR registration.relay_version ILIKE '%%' || $%[1]d || '%%'
		      OR registration.registration_id::text ILIKE '%%' || $%[1]d || '%%')`, query.Search)
	}
	if query.Version != "" {
		add("registration.relay_version = $%d", query.Version)
	}
	if query.Capability != "" {
		add(`registration.capabilities @> jsonb_build_array(jsonb_build_object('id', $%d::text))`,
			query.Capability)
	}
	if clause := relayStateClause(query.State); clause != "" {
		where = append(where, clause)
	}
	if cursorID != nil {
		arguments = append(arguments, cursorValue, *cursorID)
		comparison := "<"
		if !query.Descending {
			comparison = ">"
		}
		where = append(where, fmt.Sprintf("(%s, registration.registration_id) %s ($%d::%s, $%d::uuid)",
			ordering.column, comparison, len(arguments)-1, ordering.cast, len(arguments)))
	}

	direction := "DESC"
	if !query.Descending {
		direction = "ASC"
	}
	arguments = append(arguments, limit+1)

	statement := fmt.Sprintf(`
		SELECT registration.registration_id, registration.cluster_fingerprint,
		       registration.relay_version, registration.protocol_version,
		       registration.created_at, registration.revoked_at,
		       registration.session_conflict_at, registration.session_conflict_hosts,
		       registration.last_seen_at, registration.session_peer,
		       %s AS connected,
		       COALESCE(
		           (SELECT jsonb_agg(DISTINCT entry->>'id')
		              FROM jsonb_array_elements(registration.capabilities) AS entry),
		           '[]'::jsonb) AS advertised
		  FROM relay_registration registration
		 WHERE %s
		 ORDER BY %s %s NULLS LAST, registration.registration_id %s
		 LIMIT $%d`,
		relayConnectedExpression, strings.Join(where, "\n   AND "),
		ordering.column, direction, direction, len(arguments))

	rows, err := pool.Query(ctx, statement, arguments...)
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
			roster.Next = encodeSortCursor(scope,
				relayCursorValue(last, query.SortField, query.Descending),
				last.RegistrationID)
			break
		}
		roster.Relays = append(roster.Relays, summary)
	}
	if err = rows.Err(); err != nil {
		return RelayRoster{}, fmt.Errorf("listing relays: %w", err)
	}
	return roster, nil
}

// Connected state is derived because a stored value would go stale when a Relay disappears.
const relayConnectedExpression = `(registration.revoked_at IS NULL
	                                   AND registration.session_ended_at IS NULL
	                                   AND registration.last_seen_at IS NOT NULL
	                                   AND registration.last_seen_at > now() - $2::interval)`

func relayStateClause(state string) string {
	switch state {
	case "connected":
		return relayConnectedExpression
	case "disconnected":
		return "NOT " + relayConnectedExpression + " AND registration.revoked_at IS NULL"
	case "revoked":
		return "registration.revoked_at IS NOT NULL"
	case "degraded":
		return "registration.session_conflict_at IS NOT NULL"
	default:
		return ""
	}
}

func relayCursorValue(summary RelaySummary, field string, descending bool) string {
	switch field {
	case "lastSeenAt":
		if summary.LastSeenAt.IsZero() {
			if descending {
				return "-infinity"
			}
			return "infinity"
		}
		return summary.LastSeenAt.Format(time.RFC3339Nano)
	case "version":
		return summary.RelayVersion
	case "fingerprint":
		return summary.ClusterFingerprint
	default:
		return summary.RegisteredAt.Format(time.RFC3339Nano)
	}
}

func scanRelaySummary(rows pgx.Rows) (RelaySummary, error) {
	var (
		summary    RelaySummary
		revokedAt  *time.Time
		conflictAt *time.Time
		hosts      int
		lastSeen   *time.Time
		peer       *string
		protocol   *int64
		advertised []byte
	)
	if err := rows.Scan(&summary.RegistrationID, &summary.ClusterFingerprint,
		&summary.RelayVersion, &protocol, &summary.RegisteredAt, &revokedAt, &conflictAt, &hosts,
		&lastSeen, &peer, &summary.Connected, &advertised); err != nil {
		return RelaySummary{}, fmt.Errorf("reading a relay: %w", err)
	}
	if revokedAt != nil {
		summary.RevokedAt = *revokedAt
	}
	if conflictAt != nil {
		summary.Conflict = SessionConflict{DetectedAt: *conflictAt, DistinctHosts: hosts}
	}
	if lastSeen != nil {
		summary.LastSeenAt = *lastSeen
	}
	if peer != nil {
		summary.SessionPeer = *peer
	}
	if protocol != nil {
		summary.ProtocolVersion = uint32(*protocol)
	}
	capabilities, err := decodeStringArray(advertised)
	if err != nil {
		return RelaySummary{}, fmt.Errorf("reading a relay's advertised capabilities: %w", err)
	}
	summary.Capabilities = capabilities
	return summary, nil
}

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

// RelaySummary is a relay identity as an operator needs to see it.
//
// It carries no credential digest. Nothing an operator does with this list needs one, and a
// read model that carries a secret is that secret in one more place — in memory, in a JSON
// response, and in whatever logs or exports the response passes through afterwards.
type RelaySummary struct {
	RegistrationID     uuid.UUID
	ClusterFingerprint string
	RelayVersion       string
	// ProtocolVersion is zero for registrations created before protocol negotiation was
	// recorded. Non-zero values were accepted by the compatibility gate at enrolment.
	ProtocolVersion uint32
	RegisteredAt    time.Time
	// RevokedAt is zero for a live registration.
	RevokedAt time.Time
	Conflict  SessionConflict
	// Connected is whether this Relay is holding a session RIGHT NOW, derived from the durable
	// presence rather than from any one process's session registry. A fleet summary built from
	// the in-memory registry would report what one instance can see and call it the fleet.
	Connected bool
	// LastSeenAt is when it last proved it was alive. Zero means it has never held a session
	// since presence began being recorded, which is different from being disconnected now.
	LastSeenAt time.Time
	// SessionPeer is the host currently holding it. One host is a relay; several taking turns is
	// the credential-theft signature, and Conflict is where that is stated.
	SessionPeer string
	// Capabilities is what it advertised at enrolment, by capability id.
	Capabilities []string
}

// RelayRoster is a page of an organization's relay identities.
type RelayRoster struct {
	Relays []RelaySummary
	// Next resumes the next page, and is empty when there is none. Its presence is also how a
	// caller knows relays were left out: a truncated list that looks complete is how an
	// operator concludes a relay is gone.
	Next string
	// Total is how many rows matched before paging, when it could be counted. It is a pointer
	// because "I did not count this cheaply" is a different answer from "there are none", and a
	// fabricated count is worse than an absent one.
	Total *int
}

// RelayQuery is what a caller may narrow, order and page a fleet by. Every field is applied by
// the DATABASE: a page that is as fast at a thousand relays as at ten is one where the filtering
// happened before the rows were sent, not after.
type RelayQuery struct {
	Page Page
	// Search matches the fingerprint, the version and the identifier, because an operator with a
	// relay in front of them has whichever of those they were given.
	Search string
	// State is `connected`, `disconnected`, `revoked` or `degraded`. Empty means every state.
	State string
	// Version narrows to one exact relay version, for "what is still on the old build".
	Version string
	// Capability narrows to relays advertising a capability id.
	Capability string
	// SortField is one of registeredAt, lastSeenAt, version, fingerprint. Anything else is a
	// programming error: the handler refuses an unoffered field before it reaches here.
	SortField  string
	Descending bool
	// LivenessWindow is how recently a relay must have been heard from to count as connected.
	// It is passed in rather than assumed here, because it is the relay session's number and a
	// second copy of it would be the one that goes stale.
	LivenessWindow time.Duration
}

// relayOrderings maps a sort field to the column it orders by and the type its cursor casts to.
// It is a closed map rather than string interpolation of caller input, which is what keeps a
// sort parameter from being a way to write SQL.
var relayOrderings = map[string]struct {
	column string
	cast   string
}{
	"registeredAt": {"registration.created_at", "timestamptz"},
	"lastSeenAt":   {"registration.last_seen_at", "timestamptz"},
	"version":      {"registration.relay_version", "text"},
	"fingerprint":  {"registration.cluster_fingerprint", "text"},
}

// ListRelays returns an organization's relay identities, narrowed, ordered and paged by the
// database.
//
// It takes the principal for the same reason every operator-facing read does: the
// authorization middleware has already decided, and this is the layer that cannot be reached
// around. A principal with no membership is refused here even if it arrived from a path
// nobody routed through the middleware.
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
	if !known {
		ordering = relayOrderings["registeredAt"]
		query.Descending = true
	}
	limit := pageLimit(query.Page.Limit)
	cursorValue, cursorID, err := decodeSortCursor(query.Page.After)
	if err != nil {
		return RelayRoster{}, err
	}

	// The arguments are numbered as they are appended, so a filter added in the middle cannot
	// silently shift every placeholder after it — which is the mistake this shape exists to
	// make impossible rather than to catch in review.
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
		// JSONB containment against the roster advertised at enrolment, so the index on the
		// column can serve it rather than every row being parsed.
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

	// One more row than asked for is read, so whether anything was left out is read from the
	// data rather than guessed at by comparing a count to a limit. The position is
	// (ordering key, registration_id) rather than an offset, so a relay enrolled while an
	// operator is paging cannot shift the rows underneath them and hide one.
	//
	// A NULL ordering key sorts last in both directions, so a relay that has never held a
	// session does not lead a descending page of "least recently seen".
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
			roster.Next = encodeSortCursor(relayCursorValue(last, query.SortField),
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

// relayConnectedExpression is the one definition of "connected", written once and reused by the
// listing and the summary. Two copies of it would be two answers to the same question, and the
// one that disagreed with the list is the one an operator would be reading in the summary.
//
// It is derived rather than stored, because a stored boolean is a fact that goes stale the
// moment a process dies without clearing it.
const relayConnectedExpression = `(registration.revoked_at IS NULL
	                                   AND registration.session_ended_at IS NULL
	                                   AND registration.last_seen_at IS NOT NULL
	                                   AND registration.last_seen_at > now() - $2::interval)`

// relayStateClause narrows to one fleet state. An unrecognised value narrows nothing rather
// than refusing, because the handler has already refused anything unoffered — this is the
// second line, not the first.
func relayStateClause(state string) string {
	switch state {
	case "connected":
		return relayConnectedExpression
	case "disconnected":
		return "NOT " + relayConnectedExpression + " AND registration.revoked_at IS NULL"
	case "revoked":
		return "registration.revoked_at IS NOT NULL"
	case "degraded":
		// A relay whose identity is contested: something is holding its credential alongside it.
		// It is a fleet state rather than a footnote because it is the one state where the row
		// looks healthy and is not.
		return "registration.session_conflict_at IS NOT NULL"
	default:
		return ""
	}
}

// relayCursorValue renders the ordering key of the last row on a page, in the text form the
// cursor carries and the query casts back.
func relayCursorValue(summary RelaySummary, field string) string {
	switch field {
	case "lastSeenAt":
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

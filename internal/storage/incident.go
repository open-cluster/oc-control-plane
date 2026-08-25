package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Persistence for the operational episode Signals group into.
//
// The grouping half runs inside RecordDelivery, in the transaction that writes the Signals. An
// episode assigned afterwards would be a history that changed, and a Signal that briefly belonged
// to nothing is a Signal a reader could see ungrouped and then grouped.

// groupSignal puts one newly recorded Signal into its episode.
//
// It is called only for a Signal that was actually INSERTED. A redelivery of a firing that arrives
// after its resolution updates nothing — the guard in upsertSignal sees a resolved row and matches
// no rows — and such a delivery must open no episode either. Doing the Signal first is what makes
// that free rather than something to remember.
//
// The effective key is the source's own grouping identity where it supplied one, and the Signal's
// own alert identity where it did not. The second is not a fallback that pretends to group: it
// means one episode per alert, which is what "the source grouped nothing" honestly produces, and
// the basis recorded alongside says which of the two happened.
// It reports whether this Signal OPENED an episode or joined one that was already open. That
// distinction is the grouping outcome an operator watches: a source whose every alert opens its own
// episode is one whose group_by is not doing what its author thinks.
func groupSignal(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, signal Signal, signalID uuid.UUID,
) (uuid.UUID, bool, error) {
	key, basis := signal.GroupingKey, incident.BasisSourceGrouping
	if key == "" {
		key, basis = signal.SourceKey, incident.BasisUngrouped
	}

	episodeID, opened, err := openEpisode(
		ctx, transaction, organization, delivery, signal, key, basis)
	if err != nil {
		return uuid.Nil, false, err
	}
	if _, err = transaction.Exec(ctx,
		`UPDATE signal SET episode_id = $1 WHERE signal_id = $2 AND org_id = $3`,
		episodeID, signalID, organization.String()); err != nil {
		return uuid.Nil, false, fmt.Errorf("grouping a signal: %w", err)
	}
	return episodeID, opened, refreshEpisode(ctx, transaction, organization, episodeID)
}

// openEpisode returns the open episode for a grouping key, creating it when there is none.
//
// It is ONE statement, and that is the whole of the concurrency argument. Two deliveries carrying
// the same group arrive at once; the partial unique index decides which of them creates the row,
// and the other is handed the same row rather than being told there is none.
//
// DO UPDATE rather than DO NOTHING, and the difference is not cosmetic. DO NOTHING does not wait
// for the conflicting transaction and returns no row, so the loser would then have to SELECT — and
// would find nothing, because the winner has not committed. The delivery would fail and the source
// would retry a delivery that was never wrong. DO UPDATE takes the lock, waits, and always returns
// the row. The update itself is a touch: what the episode actually holds is recomputed afterwards
// from its own Signals.
//
// xmax is zero on a row this statement INSERTED and non-zero on one it found, which is how the
// same statement answers "did this open an episode" without a second query a concurrent delivery
// could get a different answer from.
func openEpisode(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, signal Signal, key string, basis incident.Basis,
) (uuid.UUID, bool, error) {
	// The times come from the SOURCE's clock. An episode's window is what an investigation opened
	// for it would be scoped to, so a delivery delay must not widen it.
	started := signal.StartedAt

	var (
		episodeID uuid.UUID
		opened    bool
	)
	// The conflict target repeats the index predicate because the index is partial. That is also
	// what makes a RESOLVED episode under the same key invisible here: it is a different
	// occurrence, and attaching to it would resurrect a record that has already been closed.
	err := transaction.QueryRow(ctx, `
		INSERT INTO incident_episode
			(episode_id, org_id, integration_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, signal_count)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7, 0)
		ON CONFLICT (integration_id, grouping_key) WHERE status = 1
		DO UPDATE SET updated_at = now()
		RETURNING episode_id, xmax = 0`,
		uuid.New(), organization.String(), delivery.Integration,
		key, int16(basis), signal.Title, started).Scan(&episodeID, &opened)
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("opening an incident episode: %w", err)
	}
	return episodeID, opened, nil
}

// refreshEpisode recomputes an episode from the Signals it holds.
//
// RECOMPUTED rather than incremented. A counter maintained by hand drifts the first time a write
// path is added that forgets it, and the field that drifts here is whether the failure is still
// happening — a record that says an incident recovered when it did not is the worst thing this
// table could say. The cost is one aggregate over an episode's own Signals, which is a handful of
// rows.
func refreshEpisode(
	ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, episodeID uuid.UUID,
) error {
	// An episode is resolved when NO Signal in it is still firing. The resolution time is the last
	// one to stop, because that is when the failure ended rather than when the first part of it
	// did.
	if _, err := transaction.Exec(ctx, `
		UPDATE incident_episode AS episode
		   SET signal_count  = counted.total,
		       first_seen_at = counted.first_seen,
		       last_seen_at  = counted.last_seen,
		       status        = CASE WHEN counted.firing = 0 THEN 2 ELSE 1 END,
		       resolved_at   = CASE WHEN counted.firing = 0 THEN counted.resolved END,
		       updated_at    = now()
		  FROM (
		       SELECT count(*)                                        AS total,
		              min(started_at)                                 AS first_seen,
		              max(greatest(started_at, coalesce(resolved_at, started_at))) AS last_seen,
		              count(*) FILTER (WHERE status = 1)              AS firing,
		              max(resolved_at)                                AS resolved
		         FROM signal WHERE episode_id = $1 AND org_id = $2
		       ) AS counted
		 WHERE episode.episode_id = $1 AND episode.org_id = $2 AND counted.total > 0`,
		episodeID, organization.String()); err != nil {
		return fmt.Errorf("recomputing an incident episode: %w", err)
	}
	return nil
}

// episodeColumns is what every read of an episode selects, written once because there are four
// of them and a column added to three is a field that is populated in a listing and empty in the
// read of one row.
//
// The delivering integration's NAME comes through a scalar subquery rather than a join, for two
// reasons that both matter. A LEFT JOIN would widen what `FOR UPDATE` locks, and lockEpisode uses
// this same list to read an episode for a merge — locking an unrelated integration row there
// would be a lock nobody asked for. And a subquery answers NULL where a join would drop the row
// entirely, which is the difference between an episode whose name could not be resolved and an
// episode that vanished from a listing.
const episodeColumns = `episode_id, integration_id,
		       (SELECT name FROM integration i
		         WHERE i.integration_id = e.integration_id
		           AND i.org_id = e.org_id) AS integration_name,
		       grouping_key, grouping_basis, title,
		       status, first_seen_at, last_seen_at, resolved_at, signal_count,
		       superseded_by, superseded_at, supersede_reason, created_at, updated_at`

// QueryEpisodes reports a page of a tenant's episodes.
func (p *Database) QueryEpisodes(
	ctx context.Context, organization tenancy.Organization, query incident.Query,
) (incident.Page, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Page{}, err
	}

	order, known := episodeOrderings[query.Sort]
	if !known {
		// Unreachable while the handler refuses an unoffered sort, and checked anyway: a sort
		// resolved from an empty string would order rows by whatever the planner chose, and the
		// cursor would resume from a position in an order nobody asked for.
		return incident.Page{}, fmt.Errorf("incident listing cannot order by %q", query.Sort)
	}
	cursorValue, cursorID, err := decodeSortCursor(query.Cursor)
	if err != nil {
		return incident.Page{}, incident.ErrBadCursor
	}

	arguments := []any{organization.String()}
	where := []string{"org_id = $1"}
	add := func(clause string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf(clause, len(arguments)))
	}
	if query.Integration != nil {
		add("integration_id = $%d", *query.Integration)
	}
	if query.Status != 0 {
		add("status = $%d", int16(query.Status))
	}
	if query.Search != "" {
		// Title and the source's own grouping key, because an operator arriving from their own
		// alerting holds the second and an operator reading the console holds the first.
		arguments = append(arguments, "%"+strings.ToLower(query.Search)+"%")
		where = append(where, fmt.Sprintf(
			"(lower(title) LIKE $%d OR lower(grouping_key) LIKE $%d)",
			len(arguments), len(arguments)))
	}

	direction, comparison := "ASC", ">"
	if query.Descending {
		direction, comparison = "DESC", "<"
	}
	if cursorID != nil {
		arguments = append(arguments, cursorValue, *cursorID)
		where = append(where, fmt.Sprintf("(%s, episode_id) %s ($%d::%s, $%d)",
			order.column, comparison, len(arguments)-1, order.cast, len(arguments)))
	}

	limit := pageLimit(query.Limit)
	arguments = append(arguments, limit+1)

	// One row past the limit is fetched so the cursor is issued only when there genuinely is a
	// next page. A listing that always offered one would let a caller page forever.
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT `+episodeColumns+`
		  FROM incident_episode e
		 WHERE %s
		 ORDER BY %s %s, episode_id %s
		 LIMIT $%d`,
		strings.Join(where, " AND "), order.column, direction, direction, len(arguments)),
		arguments...)
	if err != nil {
		return incident.Page{}, fmt.Errorf("reading incident episodes: %w", err)
	}
	defer rows.Close()

	var page incident.Page
	for rows.Next() {
		episode, scanErr := scanEpisode(rows, organization)
		if scanErr != nil {
			return incident.Page{}, scanErr
		}
		if len(page.Episodes) == limit {
			last := page.Episodes[len(page.Episodes)-1]
			page.Next = encodeSortCursor(order.render(last), last.ID)
			break
		}
		page.Episodes = append(page.Episodes, episode)
	}
	if err = rows.Err(); err != nil {
		return incident.Page{}, fmt.Errorf("reading incident episodes: %w", err)
	}
	return page, nil
}

// episodeOrderings is what the listing may be ordered by, with the codec for resuming each.
//
// The rendering lives beside the column deliberately: a second switch on the same field names is
// the shape of change where one edit lands and the other does not, and the symptom is a cursor
// that resumes from the wrong place rather than an error anybody sees.
var episodeOrderings = map[string]struct {
	column string
	cast   string
	render func(incident.Episode) string
}{
	"lastSeenAt": {"last_seen_at", "timestamptz", func(e incident.Episode) string {
		return e.LastSeenAt.UTC().Format(time.RFC3339Nano)
	}},
	"firstSeenAt": {"first_seen_at", "timestamptz", func(e incident.Episode) string {
		return e.FirstSeenAt.UTC().Format(time.RFC3339Nano)
	}},
	"title": {"title", "text", func(e incident.Episode) string { return e.Title }},
	"signalCount": {"signal_count", "integer", func(e incident.Episode) string {
		return fmt.Sprintf("%d", e.SignalCount)
	}},
}

// Episode reads one, scoped to the tenant.
func (p *Database) Episode(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (incident.Episode, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Episode{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT `+episodeColumns+`
		  FROM incident_episode e
		 WHERE episode_id = $1 AND org_id = $2`, id, organization.String())
	if err != nil {
		return incident.Episode{}, fmt.Errorf("reading an incident episode: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return incident.Episode{}, fmt.Errorf("reading an incident episode: %w", err)
		}
		return incident.Episode{}, incident.ErrUnknown
	}
	return scanEpisode(rows, organization)
}

// EpisodeSignals reports the Signals grouped into one episode, oldest first.
//
// Oldest first because a reader following an incident follows it forwards: what fired, then what
// fired next. Every other listing on this surface is newest first, and the difference is the point
// rather than an inconsistency.
func (p *Database) EpisodeSignals(
	ctx context.Context, organization tenancy.Organization,
	id uuid.UUID, page incident.SignalPage,
) (incident.SignalList, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.SignalList{}, err
	}
	// The episode is resolved first, so a caller asking for another tenant's Signals gets the
	// same answer as one asking for an episode that does not exist rather than an empty page.
	if _, err = p.Episode(ctx, organization, id); err != nil {
		return incident.SignalList{}, err
	}

	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return incident.SignalList{}, incident.ErrBadCursor
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT signal_id, title, summary, labels, status, started_at, resolved_at, received_at
		  FROM signal
		 WHERE episode_id = $1 AND org_id = $2
		   AND ($3::timestamptz IS NULL OR (started_at, signal_id) > ($3::timestamptz, $4::uuid))
		 ORDER BY started_at, signal_id
		 LIMIT $5`,
		id, organization.String(), after, afterID, limit+1)
	if err != nil {
		return incident.SignalList{}, fmt.Errorf("reading an episode's signals: %w", err)
	}
	defer rows.Close()

	var list incident.SignalList
	for rows.Next() {
		var signal incident.Signal
		var status int16
		var labels []byte
		var resolvedAt *time.Time
		if err = rows.Scan(&signal.ID, &signal.Title, &signal.Summary, &labels, &status,
			&signal.StartedAt, &resolvedAt, &signal.ReceivedAt); err != nil {
			return incident.SignalList{}, fmt.Errorf("scanning a signal: %w", err)
		}
		if err = json.Unmarshal(labels, &signal.Labels); err != nil {
			return incident.SignalList{}, fmt.Errorf("decoding signal labels: %w", err)
		}
		signal.Firing = SignalStatus(status) == SignalFiring
		if resolvedAt != nil {
			signal.ResolvedAt = *resolvedAt
		}
		if len(list.Signals) == limit {
			last := list.Signals[len(list.Signals)-1]
			list.Next = encodeCursor(last.StartedAt, last.ID)
			break
		}
		list.Signals = append(list.Signals, signal)
	}
	if err = rows.Err(); err != nil {
		return incident.SignalList{}, fmt.Errorf("reading an episode's signals: %w", err)
	}
	return list, nil
}

// MergeEpisodes records that two episodes are one incident.
//
// NOTHING IS REWRITTEN. The absorbed episode keeps its identity, its Signals, its grouping key and
// its own record, and gains a pointer to the one that survives it. A merge that moved Signals
// would destroy the record of the grouping it was correcting, which is the thing revisability
// exists to preserve.
//
// The Signals that arrive afterwards still land on the absorbed episode, because they still match
// its key. That is deliberate: a reader follows the pointer and sees them under the survivor, and
// freeing the key would mean the next delivery opened a third episode and the operator's decision
// quietly stopped applying.
func (p *Database) MergeEpisodes(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	merge incident.Merge,
) (incident.Episode, error) {
	if err := merge.Validate(); err != nil {
		return incident.Episode{}, fmt.Errorf("%w: %s", incident.ErrMerge, err.Error())
	}
	return audited(ctx, p, principal, organization, audit.ActionIncidentMerge,
		func(ctx context.Context, transaction pgx.Tx) (
			incident.Episode, audit.Target, audit.Detail, error,
		) {
			absorbed, err := lockEpisode(ctx, transaction, organization, merge.Absorbed)
			if err != nil {
				return incident.Episode{}, audit.Target{}, nil, err
			}
			surviving, err := lockEpisode(ctx, transaction, organization, merge.Into)
			if err != nil {
				return incident.Episode{}, audit.Target{}, nil, err
			}
			if err = mergeable(absorbed, surviving); err != nil {
				return incident.Episode{}, audit.Target{}, nil, err
			}

			if _, err = transaction.Exec(ctx, `
				UPDATE incident_episode
				   SET superseded_by = $1, superseded_at = now(), supersede_reason = $2,
				       updated_at = now()
				 WHERE episode_id = $3 AND org_id = $4`,
				surviving.ID, merge.Reason, absorbed.ID, organization.String()); err != nil {
				return incident.Episode{}, audit.Target{}, nil,
					fmt.Errorf("merging incident episodes: %w", err)
			}

			// The SURVIVOR is returned, because that is what the operator now reads the incident
			// as. The audit event names it too, alongside the one that gave way.
			after, err := readEpisode(ctx, transaction, organization, surviving.ID)
			if err != nil {
				return incident.Episode{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetIncidentEpisode, ID: absorbed.ID.String()},
				audit.Detail{
					"mergedInto": surviving.ID.String(),
					// The reason is an operator's own words about a grouping, which is exactly what
					// an auditor asking "why are these one incident" needs. It carries no
					// credential and no evidence content.
					"reason": merge.Reason,
				}, nil
		})
}

// mergeable refuses a merge that would not mean anything, with the reason.
func mergeable(absorbed, surviving incident.Episode) error {
	switch {
	case absorbed.Superseded():
		return fmt.Errorf("%w: %s has already been merged into %s",
			incident.ErrMerge, absorbed.ID, absorbed.SupersededBy)
	case surviving.Superseded():
		// One hop, never a chain. A reader that had to walk a chain would find a different answer
		// depending on where it started, and a cycle would be a read that never ends.
		return fmt.Errorf("%w: %s has itself been merged into %s; merge into that one instead",
			incident.ErrMerge, surviving.ID, surviving.SupersededBy)
	default:
		return nil
	}
}

// lockEpisode reads one episode for update, so two operators merging at once cannot both decide
// the other is still unmerged.
func lockEpisode(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) (incident.Episode, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+episodeColumns+`
		  FROM incident_episode e
		 WHERE episode_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization.String())
	if err != nil {
		return incident.Episode{}, fmt.Errorf("reading an incident episode: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return incident.Episode{}, fmt.Errorf("reading an incident episode: %w", err)
		}
		return incident.Episode{}, incident.ErrUnknown
	}
	return scanEpisode(rows, organization)
}

func readEpisode(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) (incident.Episode, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+episodeColumns+`
		  FROM incident_episode e
		 WHERE episode_id = $1 AND org_id = $2`, id, organization.String())
	if err != nil {
		return incident.Episode{}, fmt.Errorf("reading an incident episode: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return incident.Episode{}, incident.ErrUnknown
	}
	return scanEpisode(rows, organization)
}

func scanEpisode(rows pgx.Rows, organization tenancy.Organization) (incident.Episode, error) {
	var (
		episode      incident.Episode
		basis        int16
		status       int16
		resolvedAt   *time.Time
		supersededAt *time.Time
		// Nullable because the subquery that resolves it can answer nothing. Scanned into
		// a pointer rather than a string so "no name was resolved" cannot be mistaken for
		// an integration named the empty string, which the table's own check forbids.
		integrationName *string
	)
	if err := rows.Scan(&episode.ID, &episode.Integration, &integrationName,
		&episode.GroupingKey, &basis, &episode.Title, &status, &episode.FirstSeenAt,
		&episode.LastSeenAt, &resolvedAt, &episode.SignalCount,
		&episode.SupersededBy, &supersededAt, &episode.SupersedeReason,
		&episode.CreatedAt, &episode.UpdatedAt); err != nil {
		return incident.Episode{}, fmt.Errorf("scanning an incident episode: %w", err)
	}
	if integrationName != nil {
		episode.IntegrationName = *integrationName
	}
	episode.Organization = organization.String()
	episode.Basis = incident.Basis(basis)
	episode.Status = incident.Status(status)
	if resolvedAt != nil {
		episode.ResolvedAt = *resolvedAt
	}
	if supersededAt != nil {
		episode.SupersededAt = *supersededAt
	}
	return episode, nil
}

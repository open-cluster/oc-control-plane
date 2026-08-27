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
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/incident"
)

// Persistence for the operational incident AlertEvents group into.
//
// The grouping half runs inside RecordDelivery, in the transaction that writes the AlertEvents. An
// incident assigned afterwards would be a history that changed, and a AlertEvent that briefly belonged
// to nothing is a AlertEvent a reader could see ungrouped and then grouped.

// groupAlertEvent puts one newly recorded AlertEvent into its incident.
//
// It is called only for a AlertEvent that was actually INSERTED. A redelivery of a firing that arrives
// after its resolution updates nothing — the guard in upsertAlertEvent sees a resolved row and matches
// no rows — and such a delivery must open no incident either. Doing the AlertEvent first is what makes
// that free rather than something to remember.
//
// The effective key is the source's own grouping identity where it supplied one, and the AlertEvent's
// own alert identity where it did not. The second is not a fallback that pretends to group: it
// means one incident per alert, which is what "the source grouped nothing" honestly produces, and
// the basis recorded alongside says which of the two happened.
// It reports whether this AlertEvent OPENED an incident or joined one that was already open. That
// distinction is the grouping outcome an operator watches: a source whose every alert opens its own
// incident is one whose group_by is not doing what its author thinks.
func groupAlertEvent(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, alertEvent AlertEvent, alertEventID uuid.UUID,
) (uuid.UUID, bool, error) {
	key, basis := alertEvent.GroupingKey, incident.BasisSourceGrouping
	if key == "" {
		key, basis = alertEvent.SourceKey, incident.BasisUngrouped
	}

	incidentID, opened, err := openIncident(
		ctx, transaction, organization, delivery, alertEvent, key, basis)
	if err != nil {
		return uuid.Nil, false, err
	}
	if _, err = transaction.Exec(ctx,
		`UPDATE alert_event SET incident_id = $1 WHERE alert_event_id = $2 AND org_id = $3`,
		incidentID, alertEventID, organization.String()); err != nil {
		return uuid.Nil, false, fmt.Errorf("grouping a alert_event: %w", err)
	}
	return incidentID, opened, refreshIncident(ctx, transaction, organization, incidentID)
}

// openIncident returns the open incident for a grouping key, creating it when there is none.
//
// It is ONE statement, and that is the whole of the concurrency argument. Two deliveries carrying
// the same group arrive at once; the partial unique index decides which of them creates the row,
// and the other is handed the same row rather than being told there is none.
//
// DO UPDATE rather than DO NOTHING, and the difference is not cosmetic. DO NOTHING does not wait
// for the conflicting transaction and returns no row, so the loser would then have to SELECT — and
// would find nothing, because the winner has not committed. The delivery would fail and the source
// would retry a delivery that was never wrong. DO UPDATE takes the lock, waits, and always returns
// the row. The update itself is a touch: what the incident actually holds is recomputed afterwards
// from its own AlertEvents.
//
// xmax is zero on a row this statement INSERTED and non-zero on one it found, which is how the
// same statement answers "did this open an incident" without a second query a concurrent delivery
// could get a different answer from.
func openIncident(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	delivery Delivery, alertEvent AlertEvent, key string, basis incident.Basis,
) (uuid.UUID, bool, error) {
	// The times come from the SOURCE's clock. An incident's window is what an investigation opened
	// for it would be scoped to, so a delivery delay must not widen it.
	started := alertEvent.StartedAt

	var (
		incidentID uuid.UUID
		opened     bool
	)
	// The conflict target repeats the index predicate because the index is partial. That is also
	// what makes a RESOLVED incident under the same key invisible here: it is a different
	// occurrence, and attaching to it would resurrect a record that has already been closed.
	err := transaction.QueryRow(ctx, `
		INSERT INTO incident
			(incident_id, org_id, integration_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, alert_event_count)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7, 0)
		ON CONFLICT (integration_id, grouping_key) WHERE status = 1
		DO UPDATE SET updated_at = now()
		RETURNING incident_id, xmax = 0`,
		uuid.New(), organization.String(), delivery.Integration,
		key, int16(basis), alertEvent.Title, started).Scan(&incidentID, &opened)
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("opening an incident incident: %w", err)
	}
	return incidentID, opened, nil
}

// refreshIncident recomputes an incident from the AlertEvents it holds.
//
// RECOMPUTED rather than incremented. A counter maintained by hand drifts the first time a write
// path is added that forgets it, and the field that drifts here is whether the failure is still
// happening — a record that says an incident recovered when it did not is the worst thing this
// table could say. The cost is one aggregate over an incident's own AlertEvents, which is a handful of
// rows.
func refreshIncident(
	ctx context.Context, transaction pgx.Tx,
	organization tenancy.Organization, incidentID uuid.UUID,
) error {
	// An incident is resolved when NO AlertEvent in it is still firing. The resolution time is the last
	// one to stop, because that is when the failure ended rather than when the first part of it
	// did.
	if _, err := transaction.Exec(ctx, `
		UPDATE incident AS incident
		   SET alert_event_count  = counted.total,
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
		         FROM alert_event WHERE incident_id = $1 AND org_id = $2
		       ) AS counted
		 WHERE incident.incident_id = $1 AND incident.org_id = $2 AND counted.total > 0`,
		incidentID, organization.String()); err != nil {
		return fmt.Errorf("recomputing an incident incident: %w", err)
	}
	return nil
}

// episodeColumns is what every read of an incident selects, written once because there are four
// of them and a column added to three is a field that is populated in a listing and empty in the
// read of one row.
//
// The delivering integration's NAME comes through a scalar subquery rather than a join, for two
// reasons that both matter. A LEFT JOIN would widen what `FOR UPDATE` locks, and lockIncident uses
// this same list to read an incident for a merge — locking an unrelated integration row there
// would be a lock nobody asked for. And a subquery answers NULL where a join would drop the row
// entirely, which is the difference between an incident whose name could not be resolved and an
// incident that vanished from a listing.
const incidentColumns = `incident_id, integration_id,
		       (SELECT name FROM integration i
		         WHERE i.integration_id = e.integration_id
		           AND i.org_id = e.org_id) AS integration_name,
		       grouping_key, grouping_basis, title,
		       status, first_seen_at, last_seen_at, resolved_at, alert_event_count,
		       superseded_by, superseded_at, supersede_reason, created_at, updated_at`

// QueryIncidents reports a page of a tenant's incidents.
func (p *Database) QueryIncidents(
	ctx context.Context, organization tenancy.Organization, query incident.Query,
) (incident.Page, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Page{}, err
	}

	order, known := incidentOrderings[query.Sort]
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
		where = append(where, fmt.Sprintf("(%s, incident_id) %s ($%d::%s, $%d)",
			order.column, comparison, len(arguments)-1, order.cast, len(arguments)))
	}

	limit := pageLimit(query.Limit)
	arguments = append(arguments, limit+1)

	// One row past the limit is fetched so the cursor is issued only when there genuinely is a
	// next page. A listing that always offered one would let a caller page forever.
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE %s
		 ORDER BY %s %s, incident_id %s
		 LIMIT $%d`,
		strings.Join(where, " AND "), order.column, direction, direction, len(arguments)),
		arguments...)
	if err != nil {
		return incident.Page{}, fmt.Errorf("reading incident incidents: %w", err)
	}
	defer rows.Close()

	var page incident.Page
	for rows.Next() {
		item, scanErr := scanIncident(rows, organization)
		if scanErr != nil {
			return incident.Page{}, scanErr
		}
		if len(page.Incidents) == limit {
			last := page.Incidents[len(page.Incidents)-1]
			page.Next = encodeSortCursor(order.render(last), last.ID)
			break
		}
		page.Incidents = append(page.Incidents, item)
	}
	if err = rows.Err(); err != nil {
		return incident.Page{}, fmt.Errorf("reading incident incidents: %w", err)
	}
	return page, nil
}

// episodeOrderings is what the listing may be ordered by, with the codec for resuming each.
//
// The rendering lives beside the column deliberately: a second switch on the same field names is
// the shape of change where one edit lands and the other does not, and the symptom is a cursor
// that resumes from the wrong place rather than an error anybody sees.
var incidentOrderings = map[string]struct {
	column string
	cast   string
	render func(incident.Incident) string
}{
	"lastSeenAt": {"last_seen_at", "timestamptz", func(e incident.Incident) string {
		return e.LastSeenAt.UTC().Format(time.RFC3339Nano)
	}},
	"firstSeenAt": {"first_seen_at", "timestamptz", func(e incident.Incident) string {
		return e.FirstSeenAt.UTC().Format(time.RFC3339Nano)
	}},
	"title": {"title", "text", func(e incident.Incident) string { return e.Title }},
	"alertEventCount": {"alert_event_count", "integer", func(e incident.Incident) string {
		return fmt.Sprintf("%d", e.AlertEventCount)
	}},
}

// Incident reads one, scoped to the tenant.
func (p *Database) Incident(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (incident.Incident, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Incident{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2`, id, organization.String())
	if err != nil {
		return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
		}
		return incident.Incident{}, incident.ErrUnknown
	}
	return scanIncident(rows, organization)
}

// IncidentAlertEvents reports the AlertEvents grouped into one incident, oldest first.
//
// Oldest first because a reader following an incident follows it forwards: what fired, then what
// fired next. Every other listing on this surface is newest first, and the difference is the point
// rather than an inconsistency.
func (p *Database) IncidentAlertEvents(
	ctx context.Context, organization tenancy.Organization,
	id uuid.UUID, page incident.AlertEventPage,
) (incident.AlertEventList, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.AlertEventList{}, err
	}
	// The incident is resolved first, so a caller asking for another tenant's AlertEvents gets the
	// same answer as one asking for an incident that does not exist rather than an empty page.
	if _, err = p.Incident(ctx, organization, id); err != nil {
		return incident.AlertEventList{}, err
	}

	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return incident.AlertEventList{}, incident.ErrBadCursor
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT alert_event_id, title, summary, labels, status, started_at, resolved_at, received_at
		  FROM alert_event
		 WHERE incident_id = $1 AND org_id = $2
		   AND ($3::timestamptz IS NULL OR (started_at, alert_event_id) > ($3::timestamptz, $4::uuid))
		 ORDER BY started_at, alert_event_id
		 LIMIT $5`,
		id, organization.String(), after, afterID, limit+1)
	if err != nil {
		return incident.AlertEventList{}, fmt.Errorf("reading an incident's alertEvents: %w", err)
	}
	defer rows.Close()

	var list incident.AlertEventList
	for rows.Next() {
		var alertEvent incident.AlertEvent
		var status int16
		var labels []byte
		var resolvedAt *time.Time
		if err = rows.Scan(&alertEvent.ID, &alertEvent.Title, &alertEvent.Summary, &labels, &status,
			&alertEvent.StartedAt, &resolvedAt, &alertEvent.ReceivedAt); err != nil {
			return incident.AlertEventList{}, fmt.Errorf("scanning a alert_event: %w", err)
		}
		if err = json.Unmarshal(labels, &alertEvent.Labels); err != nil {
			return incident.AlertEventList{}, fmt.Errorf("decoding alert_event labels: %w", err)
		}
		alertEvent.Firing = AlertEventStatus(status) == AlertEventFiring
		if resolvedAt != nil {
			alertEvent.ResolvedAt = *resolvedAt
		}
		if len(list.AlertEvents) == limit {
			last := list.AlertEvents[len(list.AlertEvents)-1]
			list.Next = encodeCursor(last.StartedAt, last.ID)
			break
		}
		list.AlertEvents = append(list.AlertEvents, alertEvent)
	}
	if err = rows.Err(); err != nil {
		return incident.AlertEventList{}, fmt.Errorf("reading an incident's alertEvents: %w", err)
	}
	return list, nil
}

// MergeIncidents records that two incidents are one incident.
//
// NOTHING IS REWRITTEN. The absorbed incident keeps its identity, its AlertEvents, its grouping key and
// its own record, and gains a pointer to the one that survives it. A merge that moved AlertEvents
// would destroy the record of the grouping it was correcting, which is the thing revisability
// exists to preserve.
//
// The AlertEvents that arrive afterwards still land on the absorbed incident, because they still match
// its key. That is deliberate: a reader follows the pointer and sees them under the survivor, and
// freeing the key would mean the next delivery opened a third incident and the operator's decision
// quietly stopped applying.
func (p *Database) MergeIncidents(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	merge incident.Merge,
) (incident.Incident, error) {
	if err := merge.Validate(); err != nil {
		return incident.Incident{}, fmt.Errorf("%w: %s", incident.ErrMerge, err.Error())
	}
	return audited(ctx, p, principal, organization, audit.ActionIncidentMerge,
		func(ctx context.Context, transaction pgx.Tx) (
			incident.Incident, audit.Target, audit.Detail, error,
		) {
			absorbed, err := lockIncident(ctx, transaction, organization, merge.Absorbed)
			if err != nil {
				return incident.Incident{}, audit.Target{}, nil, err
			}
			surviving, err := lockIncident(ctx, transaction, organization, merge.Into)
			if err != nil {
				return incident.Incident{}, audit.Target{}, nil, err
			}
			if err = mergeable(absorbed, surviving); err != nil {
				return incident.Incident{}, audit.Target{}, nil, err
			}

			if _, err = transaction.Exec(ctx, `
				UPDATE incident
				   SET superseded_by = $1, superseded_at = now(), supersede_reason = $2,
				       updated_at = now()
				 WHERE incident_id = $3 AND org_id = $4`,
				surviving.ID, merge.Reason, absorbed.ID, organization.String()); err != nil {
				return incident.Incident{}, audit.Target{}, nil,
					fmt.Errorf("merging incident incidents: %w", err)
			}

			// The SURVIVOR is returned, because that is what the operator now reads the incident
			// as. The audit event names it too, alongside the one that gave way.
			after, err := readIncident(ctx, transaction, organization, surviving.ID)
			if err != nil {
				return incident.Incident{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetIncident, ID: absorbed.ID.String()},
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
func mergeable(absorbed, surviving incident.Incident) error {
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

// lockIncident reads one incident for update, so two operators merging at once cannot both decide
// the other is still unmerged.
func lockIncident(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) (incident.Incident, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization.String())
	if err != nil {
		return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
		}
		return incident.Incident{}, incident.ErrUnknown
	}
	return scanIncident(rows, organization)
}

func readIncident(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, id uuid.UUID,
) (incident.Incident, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2`, id, organization.String())
	if err != nil {
		return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return incident.Incident{}, incident.ErrUnknown
	}
	return scanIncident(rows, organization)
}

func scanIncident(rows pgx.Rows, organization tenancy.Organization) (incident.Incident, error) {
	var (
		found        incident.Incident
		basis        int16
		status       int16
		resolvedAt   *time.Time
		supersededAt *time.Time
		// Nullable because the subquery that resolves it can answer nothing. Scanned into
		// a pointer rather than a string so "no name was resolved" cannot be mistaken for
		// an integration named the empty string, which the table's own check forbids.
		integrationName *string
	)
	if err := rows.Scan(&found.ID, &found.Integration, &integrationName,
		&found.GroupingKey, &basis, &found.Title, &status, &found.FirstSeenAt,
		&found.LastSeenAt, &resolvedAt, &found.AlertEventCount,
		&found.SupersededBy, &supersededAt, &found.SupersedeReason,
		&found.CreatedAt, &found.UpdatedAt); err != nil {
		return incident.Incident{}, fmt.Errorf("scanning an incident incident: %w", err)
	}
	if integrationName != nil {
		found.IntegrationName = *integrationName
	}
	found.Organization = organization.String()
	found.Basis = incident.Basis(basis)
	found.Status = incident.Status(status)
	if resolvedAt != nil {
		found.ResolvedAt = *resolvedAt
	}
	if supersededAt != nil {
		found.SupersededAt = *supersededAt
	}
	return found, nil
}

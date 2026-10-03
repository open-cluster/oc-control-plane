package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/incident"
)

func groupAlertEvent(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	delivery Delivery, alertEvent alertevent.AlertEvent, alertEventID uuid.UUID,
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
		incidentID, alertEventID, organization); err != nil {
		return uuid.Nil, false, fmt.Errorf("grouping a alert_event: %w", err)
	}
	return incidentID, opened, refreshIncident(ctx, transaction, organization, incidentID)
}

func openIncident(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	delivery Delivery, alertEvent alertevent.AlertEvent, key string, basis incident.Basis,
) (uuid.UUID, bool, error) {
	started := alertEvent.StartedAt

	var (
		incidentID uuid.UUID
		opened     bool
	)
	err := transaction.QueryRow(ctx, `
		INSERT INTO incident
			(incident_id, org_id, integration_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7, now())
		ON CONFLICT (integration_id, grouping_key) WHERE status = 1
		DO UPDATE SET updated_at = now()
		RETURNING incident_id, xmax = 0`,
		uuid.New(), organization, delivery.Integration,
		key, int16(basis), alertEvent.Title, started).Scan(&incidentID, &opened)
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("opening an incident incident: %w", err)
	}
	return incidentID, opened, nil
}

func refreshIncident(
	ctx context.Context, transaction pgx.Tx,
	organization uuid.UUID, incidentID uuid.UUID,
) error {
	if _, err := transaction.Exec(ctx, `
		UPDATE incident AS incident
		   SET first_seen_at = counted.first_seen,
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
		incidentID, organization); err != nil {
		return fmt.Errorf("recomputing an incident incident: %w", err)
	}
	return nil
}

const incidentColumns = `incident_id, integration_id,
		       (SELECT name FROM integration i
		         WHERE i.integration_id = e.integration_id
		           AND i.org_id = e.org_id) AS integration_name,
		       grouping_key, grouping_basis, title,
		       status, first_seen_at, last_seen_at, resolved_at, ` +
	incidentAlertEventCount + ` AS alert_event_count,
		       superseded_by, superseded_at, supersede_reason, created_at, updated_at`

const incidentAlertEventCount = `(SELECT count(*)::integer FROM alert_event a
		         WHERE a.org_id = e.org_id
		           AND a.incident_id = e.incident_id)`

func (p *Database) QueryIncidents(
	ctx context.Context, organization uuid.UUID, query incident.Query,
) (incident.Page, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Page{}, err
	}

	order, known := incidentOrderings[query.Sort]
	if !known {
		return incident.Page{}, fmt.Errorf("incident listing cannot order by %q", query.Sort)
	}
	scope := sortScope(query.Sort, query.Descending)
	cursorValue, cursorID, err := decodeSortCursor(query.Cursor, scope)
	if err != nil {
		return incident.Page{}, incident.ErrBadCursor
	}

	arguments := []any{organization}
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
			page.Next = encodeSortCursor(scope, order.render(last), last.ID)
			break
		}
		page.Incidents = append(page.Incidents, item)
	}
	if err = rows.Err(); err != nil {
		return incident.Page{}, fmt.Errorf("reading incident incidents: %w", err)
	}
	return page, nil
}

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
	"alertEventCount": {incidentAlertEventCount, "integer", func(e incident.Incident) string {
		return fmt.Sprintf("%d", e.AlertEventCount)
	}},
}

func (p *Database) Incident(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) (incident.Incident, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.Incident{}, err
	}

	rows, err := pool.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2`, id, organization)
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

func (p *Database) IncidentAlertEvents(
	ctx context.Context, organization uuid.UUID,
	id uuid.UUID, page incident.AlertEventPage,
) (incident.AlertEventList, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return incident.AlertEventList{}, err
	}
	if _, err = p.Incident(ctx, organization, id); err != nil {
		return incident.AlertEventList{}, err
	}

	after, afterID, err := decodeCursor(page.After, "startedAt")
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
		id, organization, after, afterID, limit+1)
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
		alertEvent.Firing = alertevent.AlertEventStatus(status) == alertevent.AlertEventFiring
		if resolvedAt != nil {
			alertEvent.ResolvedAt = *resolvedAt
		}
		if len(list.AlertEvents) == limit {
			last := list.AlertEvents[len(list.AlertEvents)-1]
			list.Next = encodeCursor("startedAt", last.StartedAt, last.ID)
			break
		}
		list.AlertEvents = append(list.AlertEvents, alertEvent)
	}
	if err = rows.Err(); err != nil {
		return incident.AlertEventList{}, fmt.Errorf("reading an incident's alertEvents: %w", err)
	}
	return list, nil
}

func (p *Database) MergeIncidents(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
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
				surviving.ID, merge.Reason, absorbed.ID, organization); err != nil {
				return incident.Incident{}, audit.Target{}, nil,
					fmt.Errorf("merging incident incidents: %w", err)
			}

			after, err := readIncident(ctx, transaction, organization, surviving.ID)
			if err != nil {
				return incident.Incident{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetIncident, ID: absorbed.ID.String()},
				audit.Detail{
					"mergedInto": surviving.ID.String(),
					"reason":     merge.Reason,
				}, nil
		})
}

func mergeable(absorbed, surviving incident.Incident) error {
	switch {
	case absorbed.Superseded():
		return fmt.Errorf("%w: %s has already been merged into %s",
			incident.ErrMerge, absorbed.ID, absorbed.SupersededBy)
	case surviving.Superseded():
		return fmt.Errorf("%w: %s has itself been merged into %s; merge into that one instead",
			incident.ErrMerge, surviving.ID, surviving.SupersededBy)
	default:
		return nil
	}
}

func lockIncident(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID, id uuid.UUID,
) (incident.Incident, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2
		   FOR UPDATE`, id, organization)
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
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID, id uuid.UUID,
) (incident.Incident, error) {
	rows, err := transaction.Query(ctx, `
		SELECT `+incidentColumns+`
		  FROM incident e
		 WHERE incident_id = $1 AND org_id = $2`, id, organization)
	if err != nil {
		return incident.Incident{}, fmt.Errorf("reading an incident incident: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return incident.Incident{}, incident.ErrUnknown
	}
	return scanIncident(rows, organization)
}

func scanIncident(rows pgx.Rows, organization uuid.UUID) (incident.Incident, error) {
	var (
		found           incident.Incident
		basis           int16
		status          int16
		resolvedAt      *time.Time
		supersededAt    *time.Time
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

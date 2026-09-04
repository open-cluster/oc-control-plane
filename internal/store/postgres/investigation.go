package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The investigation capability owns its vocabulary; this file is its persistence.
var _ investigation.Store = (*Database)(nil)

const investigationColumns = `investigation_id, incident_id, integration_id, question,
	       conversation_id, turn, subject, window_from, window_until, status, conclusion,
	       stopped_by, error, spend_input_tokens,
	       spend_output_tokens, spend_micro_cents, created_by, created_at, concluded_at,
	       lease_worker <> '' AND lease_expires_at > now()`

// CreateInvestigation records one, born running. Opening is an operator act and lands in
// the audit record; everything the runner writes afterwards is the investigation's own
// provenance, which is a record of its own.
func (p *Database) CreateInvestigation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted investigation.NewInvestigation, maxPending int,
) (investigation.Investigation, error) {
	return audited(ctx, p, principal, organization, audit.ActionInvestigationOpened,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			if err := reserveWaitingInvestigation(ctx, transaction, organization, maxPending); err != nil {
				if errors.Is(err, ErrWebhookWorkCapacity) {
					return investigation.Investigation{}, audit.Target{}, nil, investigation.ErrQueueFull
				}
				return investigation.Investigation{}, audit.Target{}, nil, err
			}
			row := transaction.QueryRow(ctx, `
				INSERT INTO investigation (investigation_id, org_id, incident_id,
				                           integration_id, question, subject,
				                           window_from, window_until, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING `+investigationColumns,
				uuid.New(), organization.String(), nullableUUID(wanted.IncidentID),
				nullableUUID(wanted.IntegrationID), wanted.Question, wanted.Subject,
				wanted.WindowFrom, wanted.WindowUntil, wanted.CreatedBy)

			created, err := scanInvestigation(row, organization.String())
			if err != nil {
				if isForeignKeyViolation(err) {
					return investigation.Investigation{}, audit.Target{}, nil,
						investigation.ErrIncidentUnknown
				}
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("creating an investigation: %w", err)
			}
			return created,
				audit.Target{Kind: audit.TargetInvestigation, ID: created.ID.String()},
				audit.Detail{"subject": created.Subject}, nil
		})
}

// Investigation reads one, scoped to the tenant.
func (p *Database) Investigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (investigation.Investigation, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Investigation{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation
		 WHERE investigation_id = $1 AND org_id = $2`, id, organization.String())
	found, err := scanInvestigation(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Investigation{}, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("reading an investigation: %w", err)
	}
	return found, nil
}

// InvestigationProvenance reads the sources and runs beside one investigation.
func (p *Database) InvestigationProvenance(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) ([]investigation.Source, []investigation.ToolRun, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, nil, err
	}

	sourceRows, err := pool.Query(ctx, `
		SELECT integration_id, rank, reason, selected_at
		  FROM investigation_source
		 WHERE investigation_id = $1 AND org_id = $2
		 ORDER BY rank`, id, organization.String())
	if err != nil {
		return nil, nil, fmt.Errorf("reading an investigation's sources: %w", err)
	}
	defer sourceRows.Close()

	sources := make([]investigation.Source, 0, 4)
	for sourceRows.Next() {
		var source investigation.Source
		if err := sourceRows.Scan(&source.IntegrationID, &source.Rank, &source.Reason,
			&source.SelectedAt); err != nil {
			return nil, nil, fmt.Errorf("scanning a source: %w", err)
		}
		sources = append(sources, source)
	}
	if err := sourceRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading an investigation's sources: %w", err)
	}

	runRows, err := pool.Query(ctx, `
		SELECT integration_id, ordinal, tool, purpose, hypothesis_id, arguments,
		       window_from, window_until, outcome, truncated, summary, sources, error,
		       started_at, finished_at
		  FROM investigation_tool_run
		 WHERE investigation_id = $1 AND org_id = $2
		 ORDER BY ordinal`, id, organization.String())
	if err != nil {
		return nil, nil, fmt.Errorf("reading an investigation's runs: %w", err)
	}
	defer runRows.Close()

	runs := make([]investigation.ToolRun, 0, 8)
	for runRows.Next() {
		var (
			run           investigation.ToolRun
			integrationID *uuid.UUID
			arguments     []byte
			runSources    []byte
		)
		if err := runRows.Scan(&integrationID, &run.Ordinal,
			&run.Tool, &run.Purpose, &run.HypothesisID, &arguments,
			&run.WindowFrom, &run.WindowUntil, &run.Outcome,
			&run.Truncated, &run.Summary, &runSources, &run.Error,
			&run.StartedAt, &run.FinishedAt); err != nil {
			return nil, nil, fmt.Errorf("scanning a tool run: %w", err)
		}
		if integrationID != nil {
			run.IntegrationID = *integrationID
		}
		if len(arguments) > 0 {
			if err := json.Unmarshal(arguments, &run.Arguments); err != nil {
				return nil, nil, fmt.Errorf("decoding a run's arguments: %w", err)
			}
		}
		if len(runSources) > 0 {
			if err := json.Unmarshal(runSources, &run.Sources); err != nil {
				return nil, nil, fmt.Errorf("decoding a run's sources: %w", err)
			}
		}
		runs = append(runs, run)
	}
	if err := runRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading an investigation's runs: %w", err)
	}
	return sources, runs, nil
}

// QueryInvestigations reports a page, newest first.
func (p *Database) QueryInvestigations(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	query investigation.Query,
) (investigation.List, error) {
	page := query.Page
	if !principal.MemberOf(organization) {
		return investigation.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.List{}, err
	}
	limit := pageLimit(page.Limit)
	cursorAt, cursorID, err := decodeCursor(page.After, "-createdAt")
	if err != nil {
		return investigation.List{}, investigation.ErrBadCursor
	}

	arguments := []any{organization.String(), limit + 1}
	cursor := ""
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		cursor = "AND (created_at, investigation_id) < ($3, $4)"
	}
	// The incident narrows the same org-scoped read. It is appended AFTER the cursor so
	// the placeholder numbers do not depend on whether a page position was supplied.
	incident := ""
	if query.IncidentID != uuid.Nil {
		arguments = append(arguments, query.IncidentID)
		incident = fmt.Sprintf("AND incident_id = $%d", len(arguments))
	}

	rows, err := pool.Query(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation
		 WHERE org_id = $1 `+cursor+` `+incident+`
		 ORDER BY created_at DESC, investigation_id DESC
		 LIMIT $2`, arguments...)
	if err != nil {
		return investigation.List{}, fmt.Errorf("listing investigations: %w", err)
	}
	defer rows.Close()

	list := investigation.List{
		Investigations: make([]investigation.Investigation, 0, limit),
	}
	for rows.Next() {
		found, scanErr := scanInvestigation(rows, organization.String())
		if scanErr != nil {
			return investigation.List{}, scanErr
		}
		if len(list.Investigations) == limit {
			last := list.Investigations[limit-1]
			list.Next = encodeCursor("-createdAt", last.CreatedAt, last.ID)
			break
		}
		list.Investigations = append(list.Investigations, found)
	}
	if err := rows.Err(); err != nil {
		return investigation.List{}, fmt.Errorf("listing investigations: %w", err)
	}
	return list, nil
}

// RecordSource writes one offered source.
func (p *Database) RecordSource(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	source investigation.Source,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO investigation_source (investigation_id, org_id, integration_id,
		                                  rank, reason, selected_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, organization.String(), source.IntegrationID, source.Rank, source.Reason,
		source.SelectedAt)
	if err != nil {
		return fmt.Errorf("recording a routed source: %w", err)
	}
	return nil
}

// RecordToolRun writes one execution as it finished.
func (p *Database) RecordToolRun(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	run investigation.ToolRun,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	arguments, err := json.Marshal(orEmptyConfiguration(run.Arguments))
	if err != nil {
		return fmt.Errorf("encoding a run's arguments: %w", err)
	}
	sources, err := json.Marshal(orEmptyStrings(run.Sources))
	if err != nil {
		return fmt.Errorf("encoding a run's sources: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO investigation_tool_run (investigation_id, org_id,
		                                    integration_id, ordinal, tool,
		                                    purpose, hypothesis_id, arguments,
		                                    window_from, window_until,
		                                    outcome, truncated, summary, sources, error,
		                                    started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		id, organization.String(), nullableUUID(run.IntegrationID), run.Ordinal,
		run.Tool, run.Purpose, run.HypothesisID, arguments, run.WindowFrom, run.WindowUntil,
		int16(run.Outcome), run.Truncated, run.Summary, sources, run.Error,
		run.StartedAt, run.FinishedAt)
	if err != nil {
		return fmt.Errorf("recording a tool run: %w", err)
	}
	return nil
}

// ConcludeInvestigation ends one with its concluding document and spend. stoppedBy
// names the ceiling that forced the concluding turn, empty when the model concluded
// freely.
func (p *Database) ConcludeInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	conclusion investigation.Conclusion, stoppedBy string, spend investigation.Spend,
) error {
	encoded, err := json.Marshal(conclusion)
	if err != nil {
		return fmt.Errorf("encoding conclusion: %w", err)
	}
	return p.endInvestigation(ctx, organization, id, int16(investigation.StatusConcluded),
		encoded, stoppedBy, "", spend)
}

// FailInvestigation ends one with the reason it could not conclude.
func (p *Database) FailInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	reason string, spend investigation.Spend,
) error {
	return p.endInvestigation(ctx, organization, id, int16(investigation.StatusFailed),
		[]byte("{}"), "", reason, spend)
}

// CancelInvestigation ends active work and records the operator action atomically.
func (p *Database) CancelInvestigation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization, id uuid.UUID,
) (investigation.Investigation, error) {
	return audited(ctx, p, principal, organization, audit.ActionInvestigationCancelled,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			row := transaction.QueryRow(ctx, `
				UPDATE investigation
				   SET status = $3,
				       concluded_at = now(),
				       cancel_requested_at = now(),
				       cancelled_by = $4,
				       lease_worker = '',
				       lease_expires_at = NULL
				 WHERE investigation_id = $1 AND org_id = $2 AND status = 1
				RETURNING `+investigationColumns,
				id, organization.String(), int16(investigation.StatusCancelled), principal.ID())
			ended, err := scanInvestigation(row, organization.String())
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if checkErr := transaction.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM investigation WHERE investigation_id = $1 AND org_id = $2)`,
					id, organization.String()).Scan(&exists); checkErr != nil {
					return investigation.Investigation{}, audit.Target{}, nil, checkErr
				}
				if exists {
					return investigation.Investigation{}, audit.Target{}, nil, investigation.ErrAlreadyEnded
				}
				return investigation.Investigation{}, audit.Target{}, nil, investigation.ErrUnknown
			}
			if err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("cancelling an investigation: %w", err)
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE relay_job
				   SET status = CASE WHEN status = 0 THEN 4 ELSE status END,
				       terminal_at = CASE WHEN status = 0 THEN now() ELSE terminal_at END,
				       cancel_requested_at = coalesce(cancel_requested_at, now())
				 WHERE org_id = $1 AND investigation_id = $2 AND status IN (0, 1)`,
				organization.String(), id); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("cancelling investigation-owned Relay work: %w", err)
			}
			if _, err = transaction.Exec(ctx, `
				INSERT INTO investigation_event
				    (investigation_id, org_id, sequence, at, type, payload)
				SELECT $1, $2, coalesce(max(sequence), 0) + 1, now(), $3,
				       jsonb_build_object('message', 'Investigation cancelled by an operator')
				  FROM investigation_event
				 WHERE investigation_id = $1 AND org_id = $2`,
				id, organization.String(), int16(investigation.EventCancelled)); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("recording an investigation cancellation event: %w", err)
			}
			return ended, audit.Target{Kind: audit.TargetInvestigation, ID: id.String()},
				audit.Detail{"subject": ended.Subject}, nil
		})
}

// endInvestigation is the one write both endings share. Guarded on the row still
// running, so an investigation cannot be ended twice.
func (p *Database) endInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	status int16, conclusion []byte, stoppedBy, reason string,
	spend investigation.Spend,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation
		   SET status              = $3,
		       conclusion          = $4,
		       stopped_by          = $5,
		       error               = $6,
		       spend_input_tokens  = $7,
		       spend_output_tokens = $8,
		       spend_micro_cents   = $9,
		       concluded_at        = now(),
		       -- The lease goes with the ending. A terminal investigation is nobody's to
		       -- hold, and leaving one behind would make the sweeper reason about work
		       -- that is already finished.
		       lease_worker        = '',
		       lease_expires_at    = NULL
		 WHERE investigation_id = $1 AND org_id = $2 AND status = 1`,
		id, organization.String(), status, conclusion, stoppedBy,
		reason, spend.InputTokens, spend.OutputTokens, spend.MicroCents)
	if err != nil {
		return fmt.Errorf("ending an investigation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return investigation.ErrUnknown
	}
	return nil
}

// triggerColumns joins an incident with its newest alert_event's labels, annotations and
// generator URL: the labels are the terms routing and subject inference match on, and
// the annotations carry the operator's own runbook and dashboard links — held context
// the autonomous orientation renders. The incident row carries none of them itself.
const triggerColumns = `e.incident_id, e.integration_id, e.title, e.status,
	       e.first_seen_at, e.last_seen_at, coalesce(s.labels, '{}'::jsonb),
	       coalesce(s.annotations, '{}'::jsonb), coalesce(s.generator_url, '')
	  FROM incident e
	  LEFT JOIN LATERAL (
	      SELECT labels, annotations, generator_url FROM alert_event
	       WHERE alert_event.incident_id = e.incident_id
	       ORDER BY started_at DESC
	       LIMIT 1
	  ) s ON true`

// TriggerIncident reads what an incident contributes to the investigation it starts.
func (p *Database) TriggerIncident(
	ctx context.Context, organization tenancy.Organization, incident uuid.UUID,
) (investigation.Trigger, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Trigger{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+triggerColumns+`
		 WHERE e.incident_id = $1 AND e.org_id = $2`, incident, organization.String())
	trigger, err := scanTrigger(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Trigger{}, investigation.ErrIncidentUnknown
	}
	if err != nil {
		return investigation.Trigger{}, fmt.Errorf("reading a trigger incident: %w", err)
	}
	return trigger, nil
}

// OpenTriggers reports the organization's open incidents, newest activity first, for
// inferring a question's subject.
func (p *Database) OpenTriggers(
	ctx context.Context, organization tenancy.Organization, limit int,
) ([]investigation.Trigger, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT `+triggerColumns+`
		 WHERE e.org_id = $1 AND e.status = 1 AND e.superseded_by IS NULL
		 ORDER BY e.last_seen_at DESC
		 LIMIT $2`, organization.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("listing open incidents: %w", err)
	}
	defer rows.Close()

	triggers := make([]investigation.Trigger, 0, limit)
	for rows.Next() {
		trigger, scanErr := scanTrigger(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning an open incident: %w", scanErr)
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing open incidents: %w", err)
	}
	return triggers, nil
}

// InvestigationCandidates reports the enabled integrations an investigation may be offered.
func (p *Database) InvestigationCandidates(
	ctx context.Context, organization tenancy.Organization,
) ([]integrations.Integration, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT `+integrationColumns+`
		  FROM integration
		 WHERE org_id = $1 AND disabled_at IS NULL
		 ORDER BY name`, organization.String())
	if err != nil {
		return nil, fmt.Errorf("listing investigation candidates: %w", err)
	}
	defer rows.Close()

	candidates := make([]integrations.Integration, 0, 8)
	for rows.Next() {
		candidate, scanErr := scanIntegration(rows, organization.String())
		if scanErr != nil {
			return nil, scanErr
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing investigation candidates: %w", err)
	}
	return candidates, nil
}

func scanInvestigation(row scanned, organization string) (investigation.Investigation, error) {
	var (
		found          = investigation.Investigation{OrgID: organization}
		incidentID     *uuid.UUID
		integrationID  *uuid.UUID
		conversationID *uuid.UUID
		turn           *int
		conclusion     []byte
		concludedAt    *time.Time
	)
	if err := row.Scan(&found.ID, &incidentID, &integrationID, &found.Question,
		&conversationID, &turn, &found.Subject, &found.WindowFrom, &found.WindowUntil,
		&found.Status, &conclusion, &found.StoppedBy,
		&found.Error, &found.Spend.InputTokens,
		&found.Spend.OutputTokens, &found.Spend.MicroCents, &found.CreatedBy,
		&found.CreatedAt, &concludedAt, &found.Executing); err != nil {
		return investigation.Investigation{}, err
	}
	if conversationID != nil {
		found.ConversationID = *conversationID
	}
	if turn != nil {
		found.Turn = *turn
	}
	if incidentID != nil {
		found.IncidentID = *incidentID
	}
	if integrationID != nil {
		found.IntegrationID = *integrationID
	}
	if concludedAt != nil {
		found.ConcludedAt = *concludedAt
	}
	if len(conclusion) > 0 && string(conclusion) != "{}" {
		if err := json.Unmarshal(conclusion, &found.Conclusion); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decoding conclusion: %w", err)
		}
	}
	return found, nil
}

func scanTrigger(row scanned) (investigation.Trigger, error) {
	var (
		trigger     investigation.Trigger
		status      int16
		labels      []byte
		annotations []byte
	)
	if err := row.Scan(&trigger.IncidentID, &trigger.IntegrationID, &trigger.Title,
		&status, &trigger.FirstSeenAt, &trigger.LastSeenAt, &labels, &annotations,
		&trigger.GeneratorURL); err != nil {
		return investigation.Trigger{}, err
	}
	trigger.Resolved = status == int16(incident.StatusResolved)
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &trigger.Labels); err != nil {
			return investigation.Trigger{}, fmt.Errorf("decoding trigger labels: %w", err)
		}
	}
	if len(annotations) > 0 {
		if err := json.Unmarshal(annotations, &trigger.Annotations); err != nil {
			return investigation.Trigger{}, fmt.Errorf("decoding trigger annotations: %w", err)
		}
	}
	return trigger, nil
}

func orEmptyStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

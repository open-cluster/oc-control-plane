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
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var _ investigation.HTTPStore = (*Database)(nil)

const investigationRunColumns = `r.tool_run_id, r.integration_id, coalesce(r.ordinal, 0), r.tool, r.purpose, r.arguments,
    r.window_from, r.window_until, r.outcome, r.truncated, r.summary, r.sources, r.error,
    r.started_at, r.finished_at, r.window_applied, r.model_clipped`

const investigationColumns = `investigation_id, incident_id,
	       conversation_id, turn, subject, window_from, window_until, status, conclusion,
	       stopped_by, error, spend_input_tokens,
	       spend_output_tokens, created_at, concluded_at,
	       ` + runningAgentInvestigation

const runningAgentInvestigation = `EXISTS (
	SELECT 1 FROM agent_execution e
	 WHERE e.org_id = i.org_id AND e.investigation_id = i.investigation_id AND e.status = 'running')`

func (p *Database) CreateInvestigation(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	wanted investigation.NewInvestigation, maxPending int,
) (investigation.Investigation, error) {
	return audited(ctx, p, principal, organization, audit.ActionInvestigationOpened,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			if err := reserveWaitingInvestigations(ctx, transaction, organization, maxPending, 1); err != nil {
				if errors.Is(err, ErrInvestigationCapacity) {
					return investigation.Investigation{}, audit.Target{}, nil, investigation.ErrQueueFull
				}
				return investigation.Investigation{}, audit.Target{}, nil, err
			}
			row := transaction.QueryRow(ctx, `
				INSERT INTO investigation AS i (investigation_id, org_id, incident_id,
				                                subject, window_from, window_until, requested_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				RETURNING `+investigationColumns,
				uuid.New(), organization, nullableUUID(wanted.IncidentID), wanted.Subject,
				wanted.WindowFrom, wanted.WindowUntil, nullableUUID(principal.UserID()))

			created, err := scanInvestigation(row)
			if err != nil {
				if isForeignKeyViolation(err) {
					return investigation.Investigation{}, audit.Target{}, nil,
						investigation.ErrIncidentUnknown
				}
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("creating an investigation: %w", err)
			}
			if err := queueInvestigation(ctx, transaction, organization, created.ID); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil, err
			}
			return created,
				audit.Target{Kind: audit.TargetInvestigation, ID: created.ID.String()},
				audit.Detail{"subject": created.Subject}, nil
		})
}

func (p *Database) Investigation(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) (investigation.Investigation, error) {
	return p.investigation(ctx, organization, id, false)
}

func (p *Database) WebInvestigation(
	ctx context.Context, organization, id uuid.UUID,
) (investigation.Investigation, error) {
	return p.investigation(ctx, organization, id, true)
}

func (p *Database) investigation(
	ctx context.Context, organization, id uuid.UUID, webOnly bool,
) (investigation.Investigation, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return investigation.Investigation{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation i
		 WHERE investigation_id = $1 AND org_id = $2 AND (NOT $3 OR `+webInvestigationSource+`)`, id, organization, webOnly)
	found, err := scanInvestigation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Investigation{}, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("reading an investigation: %w", err)
	}
	refs, historical := resultEvidence(found)
	missing, err := evidenceMissing(ctx, pool, organization, found.ConversationID, refs, false)
	if err == nil && !missing && len(historical) > 0 {
		missing, err = historicalEvidenceMissing(ctx, pool, organization, found.ConversationID, historical)
	}
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("checking cited evidence: %w", err)
	}
	if missing {
		found.Conclusion.MarkMissingEvidence()
	}
	return found, nil
}

func (p *Database) InvestigationToolRuns(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) ([]investigation.ToolRunRead, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}

	runRows, err := pool.Query(ctx, `
		SELECT `+investigationRunColumns+`
		  FROM investigation_tool_run r `+toolRunOwnerJoins+`
		 WHERE r.org_id = $2 AND r.outcome IS NOT NULL AND `+webInvestigationSource+`
		   AND (r.investigation_id = $1 OR r.execution_id = (
		        SELECT execution_id FROM agent_execution WHERE org_id = $2 AND investigation_id = $1))
		 ORDER BY r.ordinal, r.started_at, r.tool_run_id`, id, organization)
	if err != nil {
		return nil, fmt.Errorf("reading an investigation's runs: %w", err)
	}
	defer runRows.Close()

	runs := make([]investigation.ToolRunRead, 0, 8)
	for runRows.Next() {
		run, err := scanToolRun(runRows, nil)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := runRows.Err(); err != nil {
		return nil, fmt.Errorf("reading an investigation's runs: %w", err)
	}
	return runs, nil
}

func (p *Database) ToolRunObservation(ctx context.Context, org uuid.UUID, ref investigation.HistoricalEvidenceRef) (investigation.ToolRunRead, uuid.UUID, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return investigation.ToolRunRead{}, uuid.Nil, err
	}
	match := "r.tool_run_id = $2"
	args := []any{org, ref.ToolRunID}
	if ref.ToolRunOrdinal > 0 {
		match = "r.investigation_id = $2 AND r.ordinal = $3"
		args = []any{org, ref.InvestigationID, ref.ToolRunOrdinal}
	}
	row := pool.QueryRow(ctx, `SELECT `+investigationRunColumns+`, r.observation_text,
		coalesce(integration.name, ''), coalesce(integration.provider, ''), i.investigation_id
		FROM investigation_tool_run r `+toolRunOwnerJoins+`
		LEFT JOIN integration ON integration.org_id = r.org_id AND integration.integration_id = r.integration_id
		WHERE r.org_id = $1 AND i.investigation_id IS NOT NULL AND r.outcome IS NOT NULL
		  AND `+match+` AND `+webInvestigationSource, args...)
	var owner uuid.UUID
	run, err := scanToolRun(row, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.ToolRunRead{}, uuid.Nil, investigation.ErrUnknown
	}
	if err != nil {
		return investigation.ToolRunRead{}, uuid.Nil, fmt.Errorf("reading a Tool Run observation: %w", err)
	}
	return run, owner, nil
}

func scanToolRun(row pgx.Row, owner *uuid.UUID) (investigation.ToolRunRead, error) {
	var run investigation.ToolRunRead
	var integrationID *uuid.UUID
	var arguments, sources []byte
	fields := []any{&run.ID, &integrationID, &run.Ordinal, &run.Tool, &run.Purpose, &arguments,
		&run.WindowFrom, &run.WindowUntil, &run.Outcome, &run.Truncated, &run.Summary,
		&sources, &run.Error, &run.StartedAt, &run.FinishedAt, &run.WindowApplied, &run.ModelClipped}
	if owner != nil {
		fields = append(fields, &run.ObservationText, &run.IntegrationName, &run.IntegrationType, owner)
	}
	if err := row.Scan(fields...); err != nil {
		return run, fmt.Errorf("scanning a tool run: %w", err)
	}
	if integrationID != nil {
		run.IntegrationID = *integrationID
	}
	if err := json.Unmarshal(arguments, &run.Arguments); err != nil {
		return run, fmt.Errorf("decoding a run's arguments: %w", err)
	}
	if err := json.Unmarshal(sources, &run.Sources); err != nil {
		return run, fmt.Errorf("decoding a run's sources: %w", err)
	}
	return run, nil
}

func (p *Database) QueryInvestigations(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	query investigation.Query,
) (investigation.List, error) {
	page := query.Page
	if principal.Organization() != organization {
		return investigation.List{}, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return investigation.List{}, err
	}
	limit := pageLimit(page.Limit)
	cursorAt, cursorID, err := decodeCursor(page.After, "-createdAt")
	if err != nil {
		return investigation.List{}, investigation.ErrBadCursor
	}

	arguments := []any{organization, limit + 1}
	cursor := ""
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		cursor = "AND (created_at, investigation_id) < ($3, $4)"
	}
	incident := ""
	if query.IncidentID != uuid.Nil {
		arguments = append(arguments, query.IncidentID)
		incident = fmt.Sprintf("AND incident_id = $%d", len(arguments))
	}

	rows, err := pool.Query(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation i
		 WHERE org_id = $1 AND `+webInvestigationSource+` `+cursor+` `+incident+`
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
		found, scanErr := scanInvestigation(rows)
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

func (p *Database) CancelInvestigation(
	ctx context.Context, principal authz.Principal, organization uuid.UUID, id uuid.UUID,
) (investigation.Investigation, error) {
	payload, err := json.Marshal(investigation.CancelledPayload())
	if err != nil {
		return investigation.Investigation{}, err
	}
	return audited(ctx, p, principal, organization, audit.ActionInvestigationCancelled,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			row := transaction.QueryRow(ctx, `
				UPDATE investigation AS i
				   SET status = $3,
				       concluded_at = now(),
				       cancel_requested_at = now()
				 WHERE investigation_id = $1 AND org_id = $2 AND status = 1 AND `+webInvestigationSource+`
				RETURNING `+investigationColumns,
				id, organization, int16(investigation.StatusCancelled))
			ended, err := scanInvestigation(row)
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if checkErr := transaction.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM investigation i WHERE investigation_id = $1 AND org_id = $2 AND `+webInvestigationSource+`)`,
					id, organization).Scan(&exists); checkErr != nil {
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
			if _, err := revokeAgentExecution(ctx, transaction, organization, uuid.Nil, uuid.Nil, id); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil, fmt.Errorf("revoking the Investigation agent lease: %w", err)
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE relay_job
				   SET status = CASE WHEN status = 0 THEN 4 ELSE status END,
				       terminal_at = CASE WHEN status = 0 THEN now() ELSE terminal_at END,
				       cancel_requested_at = coalesce(cancel_requested_at, now())
				 WHERE org_id = $1 AND investigation_id = $2 AND status IN (0, 1)`,
				organization, id); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("cancelling investigation-owned Relay work: %w", err)
			}
			if _, err = transaction.Exec(ctx, `
				INSERT INTO investigation_event
				    (investigation_id, org_id, sequence, at, type, payload)
				SELECT $1, $2, coalesce(max(sequence), 0) + 1, now(), $3,
				       $4
				  FROM investigation_event
				 WHERE investigation_id = $1 AND org_id = $2`,
				id, organization, int16(investigation.EventCancelled), payload); err != nil {
				return investigation.Investigation{}, audit.Target{}, nil,
					fmt.Errorf("recording an investigation cancellation event: %w", err)
			}
			return ended, audit.Target{Kind: audit.TargetInvestigation, ID: id.String()},
				audit.Detail{"subject": ended.Subject}, nil
		})
}

func insertInvestigationReply(ctx context.Context, transaction pgx.Tx, organization, conversationID, id uuid.UUID, assistantReply string) error {
	if _, err := transaction.Exec(ctx, `
		INSERT INTO conversation_message
		    (conversation_id, org_id, sequence, role, author_id, author_name,
		     text, investigation_id, window_from, window_until)
		SELECT turn.conversation_id, turn.org_id,
		       coalesce((SELECT max(sequence) FROM conversation_message message
		                  WHERE message.org_id = turn.org_id
		                    AND message.conversation_id = turn.conversation_id), 0) + 1,
		       $3, '', '', $4, turn.investigation_id, turn.window_from, turn.window_until
		  FROM investigation turn
		 WHERE turn.investigation_id = $1 AND turn.org_id = $2
		   AND turn.conversation_id IS NOT NULL`,
		id, organization, int16(conversation.RoleAssistant), assistantReply); err != nil {
		return fmt.Errorf("recording an assistant Message: %w", err)
	}
	if _, err := transaction.Exec(ctx, `
		UPDATE conversation
		   SET last_activity_at = now()
		 WHERE conversation_id = $1 AND org_id = $2`, conversationID, organization); err != nil {
		return fmt.Errorf("stamping a completed Conversation: %w", err)
	}
	return nil
}

func appendInvestigationEvent(ctx context.Context, tx pgx.Tx, organization, id uuid.UUID, payload investigation.EventPayload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding Investigation event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation_event (investigation_id, org_id, sequence, type, payload)
		SELECT $1, $2, coalesce(max(sequence), 0) + 1, $3, $4
		  FROM investigation_event
		 WHERE investigation_id = $1 AND org_id = $2`,
		id, organization, int16(payload.EventType()), encoded); err != nil {
		return fmt.Errorf("recording Investigation event: %w", err)
	}
	return nil
}

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

func (p *Database) TriggerIncident(
	ctx context.Context, organization uuid.UUID, incident uuid.UUID,
) (investigation.Trigger, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return investigation.Trigger{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+triggerColumns+`
		 WHERE e.incident_id = $1 AND e.org_id = $2`, incident, organization)
	trigger, err := scanTrigger(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Trigger{}, investigation.ErrIncidentUnknown
	}
	if err != nil {
		return investigation.Trigger{}, fmt.Errorf("reading a trigger incident: %w", err)
	}
	return trigger, nil
}

func (p *Database) InvestigationCandidates(
	ctx context.Context, organization uuid.UUID,
) ([]integrations.Integration, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT `+integrationColumns+`
		  FROM integration
		 WHERE org_id = $1 AND NOT disconnected
		 ORDER BY name`, organization)
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

func scanInvestigation(row scanned) (investigation.Investigation, error) {
	var (
		found          investigation.Investigation
		incidentID     *uuid.UUID
		conversationID *uuid.UUID
		turn           *int
		conclusion     []byte
		concludedAt    *time.Time
	)
	if err := row.Scan(&found.ID, &incidentID, &conversationID, &turn,
		&found.Subject, &found.WindowFrom, &found.WindowUntil,
		&found.Status, &conclusion, &found.StoppedBy,
		&found.Error, &found.Usage.InputTokens,
		&found.Usage.OutputTokens, &found.CreatedAt, &concludedAt,
		&found.Claimed); err != nil {
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
	if concludedAt != nil {
		found.ConcludedAt = *concludedAt
	}
	if len(conclusion) > 0 && string(conclusion) != "{}" {
		if err := json.Unmarshal(conclusion, &found.Conclusion); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decoding conclusion: %w", err)
		}
		var err error
		found.Historical, err = investigation.ReadHistoricalCitations(conclusion, "impact", "findings", "hypotheses", "actions", "limitations")
		if err != nil {
			return investigation.Investigation{}, fmt.Errorf("reading historical Conclusion citations: %w", err)
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

var ErrInvestigationCapacity = errors.New("organization has reached its waiting investigation limit")

func reserveWaitingInvestigations(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID, maximum, requested int,
) error {
	if maximum <= 0 {
		return nil
	}
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		organization.String()); err != nil {
		return fmt.Errorf("locking organization waiting-investigation capacity: %w", err)
	}
	var waiting int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM investigation i
		 WHERE i.org_id = $1 AND i.status = 1 AND NOT `+runningAgentInvestigation,
		organization).Scan(&waiting); err != nil {
		return fmt.Errorf("counting organization waiting investigations: %w", err)
	}
	if requested > maximum-waiting {
		return ErrInvestigationCapacity
	}
	return nil
}

func lockInvestigation(ctx context.Context, tx pgx.Tx, org uuid.UUID, id uuid.UUID) error {
	var found int
	err := tx.QueryRow(ctx, `SELECT 1 FROM investigation WHERE org_id = $1 AND investigation_id = $2 FOR NO KEY UPDATE`, org, id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.ErrUnknown
	}
	return err
}

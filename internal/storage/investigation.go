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
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The investigation capability owns its vocabulary; this file is its persistence.
var _ investigation.Store = (*Placements)(nil)

const investigationColumns = `investigation_id, episode_id, integration_id, question,
	       subject, window_from, window_until, status, findings, next_steps, stopped_by,
	       error, spend_input_tokens, spend_output_tokens, spend_micro_cents,
	       created_by, created_at, concluded_at`

// CreateInvestigation records one, born running. Opening is an operator act and lands in
// the audit record; everything the runner writes afterwards is the investigation's own
// provenance, which is a record of its own.
func (p *Placements) CreateInvestigation(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted investigation.NewInvestigation,
) (investigation.Investigation, error) {
	return audited(ctx, p, principal, organization, audit.ActionInvestigationOpened,
		func(ctx context.Context, transaction pgx.Tx) (
			investigation.Investigation, audit.Target, audit.Detail, error,
		) {
			row := transaction.QueryRow(ctx, `
				INSERT INTO investigation (investigation_id, org_id, episode_id,
				                           integration_id, question, subject,
				                           window_from, window_until, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING `+investigationColumns,
				uuid.New(), organization.String(), nullableUUID(wanted.EpisodeID),
				nullableUUID(wanted.IntegrationID), wanted.Question, wanted.Subject,
				wanted.WindowFrom, wanted.WindowUntil, wanted.CreatedBy)

			created, err := scanInvestigation(row, organization.String())
			if err != nil {
				if isForeignKeyViolation(err) {
					return investigation.Investigation{}, audit.Target{}, nil,
						investigation.ErrEpisodeUnknown
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
func (p *Placements) Investigation(
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
func (p *Placements) InvestigationProvenance(
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
		SELECT integration_id, ordinal, capability, tool, arguments,
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
		if err := runRows.Scan(&integrationID, &run.Ordinal, &run.Capability,
			&run.Tool, &arguments, &run.WindowFrom, &run.WindowUntil, &run.Outcome,
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
func (p *Placements) QueryInvestigations(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	page investigation.Page,
) (investigation.List, error) {
	if !principal.MemberOf(organization) {
		return investigation.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.List{}, err
	}
	limit := pageLimit(page.Limit)
	cursorAt, cursorID, err := decodeCursor(page.After)
	if err != nil {
		return investigation.List{}, investigation.ErrBadCursor
	}

	arguments := []any{organization.String(), limit + 1}
	cursor := ""
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		cursor = "AND (created_at, investigation_id) < ($3, $4)"
	}

	rows, err := pool.Query(ctx, `
		SELECT `+investigationColumns+`
		  FROM investigation
		 WHERE org_id = $1 `+cursor+`
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
			list.Next = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		list.Investigations = append(list.Investigations, found)
	}
	if err := rows.Err(); err != nil {
		return investigation.List{}, fmt.Errorf("listing investigations: %w", err)
	}
	return list, nil
}

// RecordSource writes one router selection.
func (p *Placements) RecordSource(
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
func (p *Placements) RecordToolRun(
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
		                                    integration_id, ordinal, capability, tool,
		                                    arguments, window_from, window_until,
		                                    outcome, truncated, summary, sources, error,
		                                    started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		id, organization.String(), nullableUUID(run.IntegrationID), run.Ordinal,
		run.Capability, run.Tool, arguments, run.WindowFrom, run.WindowUntil,
		int16(run.Outcome), run.Truncated, run.Summary, sources, run.Error,
		run.StartedAt, run.FinishedAt)
	if err != nil {
		return fmt.Errorf("recording a tool run: %w", err)
	}
	return nil
}

// ConcludeInvestigation ends one with its findings and spend. stoppedBy names the
// ceiling that forced the concluding turn, empty when the model concluded freely;
// nextSteps are the conclusion's recommended actions, nil when it carries none.
func (p *Placements) ConcludeInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	findings []investigation.Finding, nextSteps []string, stoppedBy string,
	spend investigation.Spend,
) error {
	encoded, err := json.Marshal(orEmptyFindings(findings))
	if err != nil {
		return fmt.Errorf("encoding findings: %w", err)
	}
	steps, err := json.Marshal(orEmptyStrings(nextSteps))
	if err != nil {
		return fmt.Errorf("encoding next steps: %w", err)
	}
	return p.endInvestigation(ctx, organization, id, int16(investigation.StatusConcluded),
		encoded, steps, stoppedBy, "", spend)
}

// FailInvestigation ends one with the reason it could not conclude.
func (p *Placements) FailInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	reason string, spend investigation.Spend,
) error {
	return p.endInvestigation(ctx, organization, id, int16(investigation.StatusFailed),
		[]byte("[]"), []byte("[]"), "", reason, spend)
}

// endInvestigation is the one write both endings share. Guarded on the row still
// running, so an investigation cannot be ended twice.
func (p *Placements) endInvestigation(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	status int16, findings, nextSteps []byte, stoppedBy, reason string,
	spend investigation.Spend,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE investigation
		   SET status              = $3,
		       findings            = $4,
		       next_steps          = $5,
		       stopped_by          = $6,
		       error               = $7,
		       spend_input_tokens  = $8,
		       spend_output_tokens = $9,
		       spend_micro_cents   = $10,
		       concluded_at        = now()
		 WHERE investigation_id = $1 AND org_id = $2 AND status = 1`,
		id, organization.String(), status, findings, nextSteps, stoppedBy, reason,
		spend.InputTokens, spend.OutputTokens, spend.MicroCents)
	if err != nil {
		return fmt.Errorf("ending an investigation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return investigation.ErrUnknown
	}
	return nil
}

// triggerColumns joins an episode with its newest signal's labels, annotations and
// generator URL: the labels are the terms routing and subject inference match on, and
// the annotations carry the operator's own runbook and dashboard links — held context
// the autonomous orientation renders. The episode row carries none of them itself.
const triggerColumns = `e.episode_id, e.integration_id, e.title, e.status,
	       e.first_seen_at, e.last_seen_at, coalesce(s.labels, '{}'::jsonb),
	       coalesce(s.annotations, '{}'::jsonb), coalesce(s.generator_url, '')
	  FROM incident_episode e
	  LEFT JOIN LATERAL (
	      SELECT labels, annotations, generator_url FROM signal
	       WHERE signal.episode_id = e.episode_id
	       ORDER BY started_at DESC
	       LIMIT 1
	  ) s ON true`

// TriggerEpisode reads what an episode contributes to the investigation it starts.
func (p *Placements) TriggerEpisode(
	ctx context.Context, organization tenancy.Organization, episode uuid.UUID,
) (investigation.Trigger, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return investigation.Trigger{}, err
	}
	row := pool.QueryRow(ctx, `
		SELECT `+triggerColumns+`
		 WHERE e.episode_id = $1 AND e.org_id = $2`, episode, organization.String())
	trigger, err := scanTrigger(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return investigation.Trigger{}, investigation.ErrEpisodeUnknown
	}
	if err != nil {
		return investigation.Trigger{}, fmt.Errorf("reading a trigger episode: %w", err)
	}
	return trigger, nil
}

// OpenTriggers reports the organization's open episodes, newest activity first, for
// inferring a question's subject.
func (p *Placements) OpenTriggers(
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
		return nil, fmt.Errorf("listing open episodes: %w", err)
	}
	defer rows.Close()

	triggers := make([]investigation.Trigger, 0, limit)
	for rows.Next() {
		trigger, scanErr := scanTrigger(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning an open episode: %w", scanErr)
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing open episodes: %w", err)
	}
	return triggers, nil
}

// InvestigationCandidates reports the enabled integrations the router may select among.
func (p *Placements) InvestigationCandidates(
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
		found         = investigation.Investigation{OrgID: organization}
		episodeID     *uuid.UUID
		integrationID *uuid.UUID
		findings      []byte
		nextSteps     []byte
		concludedAt   *time.Time
	)
	if err := row.Scan(&found.ID, &episodeID, &integrationID, &found.Question,
		&found.Subject, &found.WindowFrom, &found.WindowUntil, &found.Status, &findings,
		&nextSteps, &found.StoppedBy, &found.Error, &found.Spend.InputTokens,
		&found.Spend.OutputTokens, &found.Spend.MicroCents, &found.CreatedBy,
		&found.CreatedAt, &concludedAt); err != nil {
		return investigation.Investigation{}, err
	}
	if len(nextSteps) > 0 {
		if err := json.Unmarshal(nextSteps, &found.NextSteps); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decoding next steps: %w", err)
		}
		if len(found.NextSteps) == 0 {
			found.NextSteps = nil
		}
	}
	if episodeID != nil {
		found.EpisodeID = *episodeID
	}
	if integrationID != nil {
		found.IntegrationID = *integrationID
	}
	if concludedAt != nil {
		found.ConcludedAt = *concludedAt
	}
	if len(findings) > 0 {
		var decoded []struct {
			Statement  string `json:"statement"`
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
			Sources    []int  `json:"sources"`
		}
		if err := json.Unmarshal(findings, &decoded); err != nil {
			return investigation.Investigation{}, fmt.Errorf("decoding findings: %w", err)
		}
		for _, finding := range decoded {
			found.Findings = append(found.Findings, investigation.Finding{
				Statement: finding.Statement, Kind: finding.Kind,
				Confidence: finding.Confidence, Sources: finding.Sources,
			})
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
	if err := row.Scan(&trigger.EpisodeID, &trigger.IntegrationID, &trigger.Title,
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

// orEmptyFindings renders findings for the JSONB column. Kind and confidence appear
// only when set, so a deterministic-loop finding keeps the exact shape it always had.
func orEmptyFindings(findings []investigation.Finding) []map[string]any {
	encoded := make([]map[string]any, 0, len(findings))
	for _, finding := range findings {
		one := map[string]any{
			"statement": finding.Statement,
			"sources":   finding.Sources,
		}
		if finding.Kind != "" {
			one["kind"] = finding.Kind
		}
		if finding.Confidence != "" {
			one["confidence"] = finding.Confidence
		}
		encoded = append(encoded, one)
	}
	return encoded
}

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
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/postmortem"
)

var _ postmortem.Store = (*Database)(nil)

const postmortemColumns = `incident_id, status, revision, document, created_at,
	updated_at, reviewed_at, reviewed_by`

func (p *Database) GenerationInput(
	ctx context.Context, organization tenancy.Organization, incidentID uuid.UUID,
) (postmortem.GenerationInput, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return postmortem.GenerationInput{}, err
	}
	input := postmortem.GenerationInput{IncidentID: incidentID}
	var status int16
	var resolvedAt *time.Time
	err = pool.QueryRow(ctx, `
		SELECT title, status, first_seen_at, resolved_at
		  FROM incident
		 WHERE incident_id = $1 AND org_id = $2`, incidentID, organization.String()).Scan(
		&input.Title, &status, &input.FirstSeenAt, &resolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return postmortem.GenerationInput{}, postmortem.ErrUnknown
	}
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem incident: %w", err)
	}
	if incident.Status(status) != incident.StatusResolved || resolvedAt == nil {
		return postmortem.GenerationInput{}, postmortem.ErrNotEligible
	}
	input.ResolvedAt = *resolvedAt

	alerts, err := pool.Query(ctx, `
		SELECT title, summary, started_at
		  FROM alert_event
		 WHERE incident_id = $1 AND org_id = $2
		 ORDER BY started_at, alert_event_id`, incidentID, organization.String())
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem alert events: %w", err)
	}
	for alerts.Next() {
		var alert postmortem.AlertEvent
		if err = alerts.Scan(&alert.Title, &alert.Summary, &alert.At); err != nil {
			alerts.Close()
			return postmortem.GenerationInput{}, fmt.Errorf("scanning postmortem alert event: %w", err)
		}
		input.AlertEvents = append(input.AlertEvents, alert)
	}
	err = alerts.Err()
	alerts.Close()
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem alert events: %w", err)
	}

	results, err := pool.Query(ctx, `
		SELECT investigation_id, conclusion
		  FROM investigation
		 WHERE incident_id = $1 AND org_id = $2 AND status = $3
		 ORDER BY created_at, investigation_id`, incidentID, organization.String(),
		int16(investigation.StatusConcluded))
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem conclusions: %w", err)
	}
	for results.Next() {
		var result postmortem.InvestigationResult
		var document []byte
		if err = results.Scan(&result.InvestigationID, &document); err != nil {
			results.Close()
			return postmortem.GenerationInput{}, fmt.Errorf("scanning postmortem conclusion: %w", err)
		}
		if len(document) > 0 && string(document) != "{}" {
			if err = json.Unmarshal(document, &result.Conclusion); err != nil {
				results.Close()
				return postmortem.GenerationInput{}, fmt.Errorf("decoding postmortem conclusion: %w", err)
			}
			input.Results = append(input.Results, result)
		}
	}
	err = results.Err()
	results.Close()
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem conclusions: %w", err)
	}

	runs, err := pool.Query(ctx, `
		SELECT r.investigation_id, r.ordinal, r.tool, r.purpose, r.hypothesis_id,
		       r.outcome, r.summary, r.started_at, r.finished_at
		  FROM investigation_tool_run r
		  JOIN investigation i
		    ON i.org_id = r.org_id AND i.investigation_id = r.investigation_id
		 WHERE i.incident_id = $1 AND r.org_id = $2
		 ORDER BY i.created_at, r.ordinal`, incidentID, organization.String())
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem tool runs: %w", err)
	}
	for runs.Next() {
		var evidence postmortem.RunEvidence
		if err = runs.Scan(&evidence.InvestigationID, &evidence.Run.Ordinal,
			&evidence.Run.Tool, &evidence.Run.Purpose, &evidence.Run.HypothesisID,
			&evidence.Run.Outcome, &evidence.Run.Summary, &evidence.Run.StartedAt,
			&evidence.Run.FinishedAt); err != nil {
			runs.Close()
			return postmortem.GenerationInput{}, fmt.Errorf("scanning postmortem tool run: %w", err)
		}
		input.Runs = append(input.Runs, evidence)
	}
	err = runs.Err()
	runs.Close()
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem tool runs: %w", err)
	}

	events, err := pool.Query(ctx, `
		SELECT e.investigation_id, e.sequence, e.at, e.type, e.payload
		  FROM investigation_event e
		  JOIN investigation i
		    ON i.org_id = e.org_id AND i.investigation_id = e.investigation_id
		 WHERE i.incident_id = $1 AND e.org_id = $2
		 ORDER BY i.created_at, e.sequence`, incidentID, organization.String())
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem events: %w", err)
	}
	for events.Next() {
		var evidence postmortem.EventEvidence
		var eventType int16
		var payload []byte
		if err = events.Scan(&evidence.InvestigationID, &evidence.Event.Sequence,
			&evidence.Event.At, &eventType, &payload); err != nil {
			events.Close()
			return postmortem.GenerationInput{}, fmt.Errorf("scanning postmortem event: %w", err)
		}
		evidence.Event.Type = investigation.EventType(eventType)
		if len(payload) > 0 {
			if err = json.Unmarshal(payload, &evidence.Event.Payload); err != nil {
				events.Close()
				return postmortem.GenerationInput{}, fmt.Errorf("decoding postmortem event: %w", err)
			}
		}
		input.Events = append(input.Events, evidence)
	}
	err = events.Err()
	events.Close()
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem events: %w", err)
	}

	messages, err := pool.Query(ctx, `
		SELECT m.actor_display, m.text, m.created_at
		  FROM conversation_message m
		  JOIN conversation c
		    ON c.org_id = m.org_id AND c.conversation_id = m.conversation_id
		 WHERE c.incident_id = $1 AND m.org_id = $2
		 ORDER BY m.created_at, m.conversation_id, m.sequence`,
		incidentID, organization.String())
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem messages: %w", err)
	}
	for messages.Next() {
		var message postmortem.Message
		if err = messages.Scan(&message.Author, &message.Text, &message.At); err != nil {
			messages.Close()
			return postmortem.GenerationInput{}, fmt.Errorf("scanning postmortem message: %w", err)
		}
		input.Messages = append(input.Messages, message)
	}
	err = messages.Err()
	messages.Close()
	if err != nil {
		return postmortem.GenerationInput{}, fmt.Errorf("reading postmortem messages: %w", err)
	}
	return input, nil
}

func (p *Database) Postmortem(
	ctx context.Context, organization tenancy.Organization, incidentID uuid.UUID,
) (postmortem.Postmortem, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return postmortem.Postmortem{}, err
	}
	return scanPostmortem(pool.QueryRow(ctx, `SELECT `+postmortemColumns+`
		FROM postmortem WHERE incident_id = $1 AND org_id = $2`,
		incidentID, organization.String()))
}

func (p *Database) CreateDraft(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	draft postmortem.Postmortem,
) (postmortem.Postmortem, error) {
	return p.writePostmortem(ctx, principal, organization, audit.ActionPostmortemCreated,
		func(ctx context.Context, tx pgx.Tx) (postmortem.Postmortem, error) {
			document, err := json.Marshal(draft)
			if err != nil {
				return postmortem.Postmortem{}, err
			}
			row := tx.QueryRow(ctx, `
				INSERT INTO postmortem (incident_id, org_id, status, revision, document)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING `+postmortemColumns,
				draft.IncidentID, organization.String(), postmortem.StatusDraft, 1, document)
			created, err := scanPostmortem(row)
			if isUniqueViolation(err, "postmortem_pkey") {
				return postmortem.Postmortem{}, postmortem.ErrAlreadyExists
			}
			return created, err
		})
}

func (p *Database) ReplaceDraft(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	draft postmortem.Postmortem,
) (postmortem.Postmortem, error) {
	return p.writePostmortem(ctx, principal, organization, audit.ActionPostmortemRegenerated,
		func(ctx context.Context, tx pgx.Tx) (postmortem.Postmortem, error) {
			document, err := json.Marshal(draft)
			if err != nil {
				return postmortem.Postmortem{}, err
			}
			return scanPostmortem(tx.QueryRow(ctx, `
				UPDATE postmortem
				   SET status = $3, revision = $4, document = $5, updated_at = now(),
				       reviewed_at = NULL, reviewed_by = ''
				 WHERE incident_id = $1 AND org_id = $2
				RETURNING `+postmortemColumns,
				draft.IncidentID, organization.String(), postmortem.StatusDraft,
				draft.Revision, document))
		})
}

func (p *Database) Correct(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	incidentID uuid.UUID, corrections postmortem.Corrections,
) (postmortem.Postmortem, error) {
	return p.writePostmortem(ctx, principal, organization, audit.ActionPostmortemCorrected,
		func(ctx context.Context, tx pgx.Tx) (postmortem.Postmortem, error) {
			current, err := scanPostmortem(tx.QueryRow(ctx, `SELECT `+postmortemColumns+`
				FROM postmortem WHERE incident_id = $1 AND org_id = $2 FOR UPDATE`,
				incidentID, organization.String()))
			if err != nil {
				return postmortem.Postmortem{}, err
			}
			if current.Status == postmortem.StatusReviewed {
				return postmortem.Postmortem{}, postmortem.ErrAlreadyReviewed
			}
			current = postmortem.ApplyCorrections(current, corrections)
			current.Revision++
			document, err := json.Marshal(current)
			if err != nil {
				return postmortem.Postmortem{}, err
			}
			return scanPostmortem(tx.QueryRow(ctx, `
				UPDATE postmortem SET revision = $3, document = $4, updated_at = now()
				 WHERE incident_id = $1 AND org_id = $2 RETURNING `+postmortemColumns,
				incidentID, organization.String(), current.Revision, document))
		})
}

func (p *Database) Review(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	incidentID uuid.UUID,
) (postmortem.Postmortem, error) {
	return p.writePostmortem(ctx, principal, organization, audit.ActionPostmortemReviewed,
		func(ctx context.Context, tx pgx.Tx) (postmortem.Postmortem, error) {
			reviewed, err := scanPostmortem(tx.QueryRow(ctx, `
				UPDATE postmortem
				   SET status = $3, reviewed_at = now(), reviewed_by = $4, updated_at = now()
				 WHERE incident_id = $1 AND org_id = $2 AND status = $5
				RETURNING `+postmortemColumns,
				incidentID, organization.String(), postmortem.StatusReviewed,
				principal.ID(), postmortem.StatusDraft))
			if errors.Is(err, postmortem.ErrUnknown) {
				current, readErr := scanPostmortem(tx.QueryRow(ctx, `SELECT `+postmortemColumns+`
					FROM postmortem WHERE incident_id = $1 AND org_id = $2`,
					incidentID, organization.String()))
				if readErr == nil && current.Status == postmortem.StatusReviewed {
					return postmortem.Postmortem{}, postmortem.ErrAlreadyReviewed
				}
			}
			return reviewed, err
		})
}

func (p *Database) writePostmortem(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	action audit.Action,
	write func(context.Context, pgx.Tx) (postmortem.Postmortem, error),
) (postmortem.Postmortem, error) {
	return audited(ctx, p, principal, organization, action,
		func(ctx context.Context, tx pgx.Tx) (postmortem.Postmortem, audit.Target,
			audit.Detail, error) {
			written, err := write(ctx, tx)
			if err != nil {
				return postmortem.Postmortem{}, audit.Target{}, nil, err
			}
			return written, audit.Target{Kind: audit.TargetPostmortem,
				ID: written.IncidentID.String()}, audit.Detail{"revision": written.Revision}, nil
		})
}

func scanPostmortem(row scanned) (postmortem.Postmortem, error) {
	var (
		found      postmortem.Postmortem
		incidentID uuid.UUID
		status     string
		revision   int
		document   []byte
		createdAt  time.Time
		updatedAt  time.Time
		reviewedAt *time.Time
		reviewedBy string
	)
	if err := row.Scan(&incidentID, &status, &revision, &document,
		&createdAt, &updatedAt, &reviewedAt, &reviewedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return postmortem.Postmortem{}, postmortem.ErrUnknown
		}
		return postmortem.Postmortem{}, err
	}
	if err := json.Unmarshal(document, &found); err != nil {
		return postmortem.Postmortem{}, fmt.Errorf("decoding postmortem: %w", err)
	}
	found.IncidentID = incidentID
	found.Status = postmortem.Status(status)
	found.Revision = revision
	found.CreatedAt = createdAt
	found.UpdatedAt = updatedAt
	found.ReviewedBy = reviewedBy
	if reviewedAt != nil {
		found.ReviewedAt = *reviewedAt
	}
	return found, nil
}

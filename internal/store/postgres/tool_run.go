package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var ErrAgentToolRunUnavailable = errors.New("agent Tool Run is unavailable")

// AgentToolAccess is the Integration and source authorized for one read.
type AgentToolAccess struct {
	Integration integrations.Integration
	Origin      *investigation.ConversationOrigin
}

// AgentToolScope revalidates the accepted input's authority under the current execution lease.
func (p *Database) AgentToolScope(
	ctx context.Context, org uuid.UUID, execution AgentExecution,
) (conversation.Window, *investigation.ConversationOrigin, error) {
	var window conversation.Window
	var origin *investigation.ConversationOrigin
	err := p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		var err error
		window, origin, err = agentToolScope(ctx, tx, org, execution)
		return err
	})
	return window, origin, err
}

func agentToolScope(
	ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution,
) (conversation.Window, *investigation.ConversationOrigin, error) {
	var window conversation.Window
	if execution.InvestigationID != uuid.Nil {
		err := tx.QueryRow(ctx, `
			SELECT window_from, window_until FROM investigation
			 WHERE org_id = $1 AND investigation_id = $2 AND automatic_incident`,
			org, execution.InvestigationID).Scan(&window.From, &window.Until)
		return window, nil, err
	}
	var author string
	if err := tx.QueryRow(ctx, `
		SELECT author_id, window_from, window_until FROM conversation_message
		 WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3 AND role = $4`,
		org, execution.ConversationID, execution.MessageSequence, int16(conversation.RoleUser)).
		Scan(&author, &window.From, &window.Until); err != nil {
		return window, nil, err
	}
	origin, err := conversationOrigin(ctx, tx, org, execution.ConversationID, true)
	if err != nil || origin != nil {
		return window, origin, err
	}
	user, err := uuid.Parse(author)
	if err != nil || user == uuid.Nil {
		return window, nil, authz.ErrNotAMember
	}
	var roleName string
	err = tx.QueryRow(ctx, `
		SELECT membership.role FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1 AND membership.user_id = $2 AND person.disabled_at IS NULL
		 FOR SHARE OF membership, person`, org, user).Scan(&roleName)
	if err != nil {
		return window, nil, err
	}
	role, known := authz.ParseRole(roleName)
	if !known || !role.Grants(authz.ConversationWrite) {
		return window, nil, authz.ErrNotAMember
	}
	return window, nil, nil
}

func checkAgentToolAccess(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution, id uuid.UUID, access *AgentToolAccess) error {
	if access == nil {
		return nil
	}
	if id != access.Integration.ID {
		return ErrAgentToolRunUnavailable
	}
	if access.Origin != nil && access.Origin.IntegrationID != id {
		// A Slack-origin read can use another provider. Fence both Integrations in the same order.
		var locked int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (
			    SELECT integration_id FROM integration
			     WHERE org_id = $1 AND integration_id IN ($2, $3)
			     ORDER BY integration_id FOR UPDATE
			) sources`, org, id, access.Origin.IntegrationID).Scan(&locked)
		if err != nil {
			return err
		}
		if locked != 2 {
			return ErrAgentToolRunUnavailable
		}
	}
	_, origin, err := agentToolScope(ctx, tx, org, execution)
	if err != nil {
		return err
	}
	if (origin == nil) != (access.Origin == nil) || (origin != nil && *origin != *access.Origin) {
		return ErrAgentToolRunUnavailable
	}
	return checkExpectedIntegration(ctx, tx, org, access.Integration)
}

// BeginAgentToolRun records the approved, bounded input before dispatch assigns work to a provider.
// Native Integration dispatch supplies its current access snapshot.
func (p *Database) BeginAgentToolRun(
	ctx context.Context, org uuid.UUID, execution AgentExecution, run investigation.ToolRun, access *AgentToolAccess,
) (uuid.UUID, error) {
	arguments, err := json.Marshal(orEmptyConfiguration(run.Arguments))
	if err != nil {
		return uuid.Nil, fmt.Errorf("encoding a Tool Run's arguments: %w", err)
	}
	if len(arguments) > investigation.MaxObservationBytes {
		return uuid.Nil, errors.New("tool run arguments exceed their storage bound")
	}
	id := uuid.New()
	err = p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		if err := checkAgentToolAccess(ctx, tx, org, execution, run.IntegrationID, access); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO investigation_tool_run
			    (org_id, tool_run_id, execution_id, integration_id, tool, purpose,
			     arguments, window_from, window_until, started_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			org, id, execution.ID, nullableUUID(run.IntegrationID), run.Tool, run.Purpose,
			arguments, run.WindowFrom, run.WindowUntil, run.StartedAt)
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// CompleteAgentToolRun writes a prepared observation once under the execution's current lease.
func (p *Database) CompleteAgentToolRun(
	ctx context.Context, org uuid.UUID, execution AgentExecution, run investigation.ToolRun, access *AgentToolAccess,
) error {
	if run.ObservationText != nil && len(*run.ObservationText) > investigation.MaxObservationBytes {
		return errors.New("tool run observation exceeds its storage bound")
	}
	sources, err := json.Marshal(orEmptyStrings(run.Sources))
	if err != nil {
		return fmt.Errorf("encoding a Tool Run's sources: %w", err)
	}
	return p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		if run.Outcome == investigation.RunSucceeded {
			if err := checkAgentToolAccess(ctx, tx, org, execution, run.IntegrationID, access); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE investigation_tool_run
			   SET outcome = $4, truncated = $5, summary = $6, sources = $7, error = $8,
			       finished_at = $9, observation_text = $10, model_clipped = $11, window_applied = $12,
			       window_from = CASE WHEN $12 THEN $13 ELSE window_from END,
			       window_until = CASE WHEN $12 THEN $14 ELSE window_until END
			 WHERE org_id = $1 AND execution_id = $2 AND tool_run_id = $3 AND outcome IS NULL`,
			org, execution.ID, run.ID, int16(run.Outcome), run.Truncated, run.Summary, sources, run.Error,
			run.FinishedAt, run.ObservationText, run.ModelClipped, run.WindowApplied, run.WindowFrom, run.WindowUntil)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrAgentToolRunUnavailable
		}
		return err
	})
}

func interruptAgentToolRuns(ctx context.Context, tx pgx.Tx, org, executionID uuid.UUID) error {
	// A later claim can replay the read but must allocate a new attempt.
	_, err := tx.Exec(ctx, `
		WITH interrupted AS (
		    UPDATE investigation_tool_run SET outcome = $3, finished_at = clock_timestamp(),
		           error = 'execution ended before Tool completion'
		     WHERE org_id = $1 AND execution_id = $2 AND outcome IS NULL
		)
		UPDATE relay_job job
		   SET status = CASE WHEN job.status = $4 THEN $6 ELSE job.status END,
		       terminal_at = CASE WHEN job.status = $4 THEN now() ELSE job.terminal_at END,
		       cancel_requested_at = coalesce(job.cancel_requested_at, now())
		  FROM investigation_tool_run run
		 WHERE job.org_id = $1 AND run.org_id = job.org_id AND run.tool_run_id = job.tool_run_id
		   AND run.execution_id = $2 AND job.status IN ($4, $5)`,
		org, executionID, int16(investigation.RunFailed),
		int16(JobPending), int16(JobLeased), int16(JobCancelled))
	return err
}

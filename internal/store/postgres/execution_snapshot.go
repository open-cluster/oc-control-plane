package storage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/execution"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const activeSnapshotLease = `(e.status = 'running' AND e.lease_expires_at > statement_timestamp())`

const snapshotExecutionStatus = `CASE WHEN e.status = 'running' AND e.lease_expires_at <= statement_timestamp()
                                     THEN 'queued' ELSE e.status END`

const snapshotTools = `
	CROSS JOIN LATERAL (
		SELECT coalesce(jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
		    'toolRunId', r.tool_run_id, 'tool', r.tool,
		    'status', CASE WHEN r.outcome = 1 THEN 'succeeded'
		                   WHEN r.active THEN 'running'
		                   WHEN r.outcome IS NULL OR r.error = '` + interruptedToolRunReason + `' THEN 'interrupted'
		                   ELSE 'failed' END,
		    'startedAt', r.started_at, 'finishedAt', r.finished_at, 'truncated', r.truncated
		)) ORDER BY r.active DESC NULLS LAST, greatest(r.started_at, r.finished_at) DESC, r.tool_run_id DESC), '[]'::jsonb) AS tools
		FROM (
			SELECT r.tool_run_id, r.tool, r.outcome, r.error, r.started_at, r.finished_at, r.truncated,
			       (r.outcome IS NULL AND ` + activeSnapshotLease + `) AS active
			FROM investigation_tool_run r`

const snapshotToolLimit = `
			ORDER BY active DESC NULLS LAST, greatest(r.started_at, r.finished_at) DESC, r.tool_run_id DESC LIMIT 32
		) r
	) activity`

const chatSnapshotQuery = `
	SELECT e.execution_id, c.conversation_id, NULL::uuid, ` + snapshotExecutionStatus + `,
	       CASE WHEN ` + activeSnapshotLease + ` THEN e.draft_text END,
	       m.sequence, m.text, activity.tools
	  FROM agent_execution e
	  JOIN conversation c ON c.org_id = e.org_id AND c.conversation_id = e.conversation_id
	  LEFT JOIN conversation_message m ON m.org_id = c.org_id AND m.conversation_id = c.conversation_id
	      AND m.sequence = e.reply_message_sequence AND m.role = 2 AND e.status = 'completed'` + snapshotTools + `
			WHERE r.org_id = e.org_id AND r.execution_id = e.execution_id` + snapshotToolLimit + `
	 WHERE e.org_id = $1 AND e.conversation_id = $2 AND e.execution_id = $3
	   AND c.source = 'web' AND c.deleted_at IS NULL`

const investigationSnapshotQuery = `
	SELECT e.execution_id, CASE WHEN c.source = 'web' AND c.deleted_at IS NULL THEN c.conversation_id END,
	       i.investigation_id,
	       CASE i.status WHEN 2 THEN 'completed' WHEN 3 THEN 'failed' WHEN 4 THEN 'cancelled'
	                     ELSE coalesce(` + snapshotExecutionStatus + `, 'queued') END,
	       NULL::text, NULL::bigint, NULL::text, activity.tools
	  FROM investigation i
	  LEFT JOIN agent_execution e ON e.org_id = i.org_id AND e.investigation_id = i.investigation_id
	  LEFT JOIN conversation c ON c.org_id = i.org_id AND c.conversation_id = i.conversation_id` + snapshotTools + `
			WHERE r.org_id = i.org_id
			  AND (r.execution_id = e.execution_id OR r.investigation_id = i.investigation_id)` + snapshotToolLimit + `
	 WHERE i.org_id = $1 AND i.investigation_id = $2 AND ` + webInvestigationSource

func (p *Database) ChatExecutionSnapshot(ctx context.Context, org, conversationID, executionID uuid.UUID) (execution.Snapshot, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return execution.Snapshot{}, err
	}
	return readExecutionSnapshot(pool.QueryRow(ctx, chatSnapshotQuery, org, conversationID, executionID), conversation.ErrUnknown)
}

func (p *Database) InvestigationExecutionSnapshot(ctx context.Context, org, id uuid.UUID) (execution.Snapshot, error) {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return execution.Snapshot{}, err
	}
	return readExecutionSnapshot(pool.QueryRow(ctx, investigationSnapshotQuery, org, id), investigation.ErrUnknown)
}

// SaveChatDraft publishes authorized provisional text without extending the worker's lease.
func (p *Database) SaveChatDraft(ctx context.Context, org uuid.UUID, execution AgentExecution, text string) error {
	if execution.ConversationID == uuid.Nil {
		return ErrAgentExecutionUnavailable
	}
	return p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		if _, _, err := agentToolScope(ctx, tx, org, execution); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE agent_execution SET draft_text = nullif($3, '')
			 WHERE org_id = $1 AND execution_id = $2 AND draft_text IS DISTINCT FROM nullif($3, '')`,
			org, execution.ID, text)
		return err
	})
}

func readExecutionSnapshot(row pgx.Row, unknown error) (execution.Snapshot, error) {
	var snapshot execution.Snapshot
	var executionID, conversationID, investigationID uuid.NullUUID
	var draft, text *string
	var sequence *int64
	var tools []byte
	err := row.Scan(&executionID, &conversationID, &investigationID, &snapshot.Status, &draft, &sequence, &text, &tools)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, unknown
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.ExecutionID, snapshot.ConversationID, snapshot.InvestigationID = executionID.UUID, conversationID.UUID, investigationID.UUID
	if draft != nil {
		snapshot.DraftText = *draft
	}
	if sequence != nil && text != nil {
		snapshot.Message = &execution.CommittedMessage{Sequence: *sequence, Text: *text}
	}
	err = json.Unmarshal(tools, &snapshot.Tools)
	return snapshot, err
}

func (p *Database) CancelChatExecution(ctx context.Context, principal authz.Principal, org, conversationID, executionID uuid.UUID) (execution.Snapshot, error) {
	var snapshot execution.Snapshot
	if principal.Organization() != org || !principal.HavePermission(authz.ConversationWrite) {
		return snapshot, authz.ErrNotAMember
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return snapshot, err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var exists int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM conversation
			WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL
			FOR NO KEY UPDATE`, org, conversationID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return conversation.ErrUnknown
			}
			return err
		}
		found, changed, err := revokeAgentExecution(ctx, tx, org, conversationID, executionID, uuid.Nil)
		if err != nil {
			return err
		}
		if !found {
			return conversation.ErrUnknown
		}
		if changed {
			if err := writeEvent(ctx, tx, audit.Event{
				Organization: org.String(), Actor: principal.Actor(), Action: audit.ActionChatExecutionCancelled,
				Target: audit.Target{Kind: audit.TargetConversation, ID: conversationID.String()}, Outcome: audit.OutcomeAllowed,
				SourceAddress: principal.SourceAddress(), RequestID: principal.RequestID(), Detail: audit.Detail{"executionId": executionID.String()},
			}); err != nil {
				return err
			}
		}
		snapshot, err = readExecutionSnapshot(tx.QueryRow(ctx, chatSnapshotQuery, org, conversationID, executionID), conversation.ErrUnknown)
		return err
	})
	return snapshot, err
}

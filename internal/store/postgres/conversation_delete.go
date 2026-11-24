package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func (p *Database) DeleteConversation(ctx context.Context, principal authz.Principal, org, id uuid.UUID) error {
	if !principal.HavePermission(authz.ConversationWrite) {
		return ErrNotAMember
	}
	_, err := audited(ctx, p, principal, org, audit.ActionConversationDeleted, func(ctx context.Context, tx pgx.Tx) (
		struct{}, audit.Target, audit.Detail, error,
	) {
		err := deleteWebConversation(ctx, tx, org, id)
		return struct{}{}, audit.Target{Kind: audit.TargetConversation, ID: id.String()}, nil, err
	})
	return err
}

func deleteWebConversation(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	var exists int
	err := tx.QueryRow(ctx, `SELECT 1 FROM conversation
		WHERE org_id = $1 AND conversation_id = $2 AND source = 'web' AND deleted_at IS NULL
		FOR NO KEY UPDATE`, org, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.ErrUnknown
	}
	if err != nil {
		return err
	}
	if err := cancelConversationWork(ctx, tx, org, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_checkpoint WHERE org_id = $1 AND conversation_id = $2`, org, id); err != nil {
		return err
	}
	// Execution and Tool Run foreign keys retain the Message identities.
	if _, err := tx.Exec(ctx, `UPDATE conversation_message
		SET text = '', author_id = '', author_name = '', source_reference = '', client_message_id = NULL
		WHERE org_id = $1 AND conversation_id = $2`, org, id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE conversation SET deleted_at = now(), title = '', created_by = ''
		WHERE org_id = $1 AND conversation_id = $2`, org, id)
	return err
}

func cancelConversationWork(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT execution_id FROM agent_execution
		WHERE org_id = $1 AND conversation_id = $2 AND status IN ('queued', 'running', 'needs_input')
		ORDER BY message_sequence`, org, id)
	if err != nil {
		return err
	}
	executions, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, executionID := range executions {
		if _, err := revokeAgentExecution(ctx, tx, org, id, executionID, uuid.Nil); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `UPDATE investigation
		SET status = $3, concluded_at = now(), cancel_requested_at = now()
		WHERE org_id = $1 AND conversation_id = $2 AND turn IS NOT NULL AND status = 1
		RETURNING investigation_id`, org, id, int16(investigation.StatusCancelled))
	if err != nil {
		return err
	}
	legacy, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, investigationID := range legacy {
		if err := cancelInvestigationWork(ctx, tx, org, investigationID); err != nil {
			return err
		}
	}
	return nil
}

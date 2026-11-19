package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var (
	ErrAgentLeaseLost            = errors.New("agent execution lease is no longer current")
	ErrAgentExecutionUnavailable = errors.New("agent execution is unavailable")
	ErrClientMessageConflict     = errors.New("client message ID already names different input")
)

type AgentExecutionStatus string

const (
	AgentExecutionNeedsInput AgentExecutionStatus = "needs_input"
	AgentExecutionCompleted  AgentExecutionStatus = "completed"
	AgentExecutionFailed     AgentExecutionStatus = "failed"
	AgentExecutionCancelled  AgentExecutionStatus = "cancelled"
)

type AgentExecution struct {
	ID                   uuid.UUID
	ConversationID       uuid.UUID
	InvestigationID      uuid.UUID
	MessageSequence      int64
	ReplyMessageSequence int64
	Status               AgentExecutionStatus
	LeaseToken           uuid.UUID
	LeaseExpiresAt       *time.Time
	StartedAt            *time.Time
}

func (execution AgentExecution) SessionID() string {
	if execution.ConversationID != uuid.Nil {
		return "conversation/" + execution.ConversationID.String()
	}
	return "execution/" + execution.ID.String()
}

const agentExecutionColumns = "e.execution_id, e.conversation_id, e.investigation_id, " +
	"coalesce(e.message_sequence, 0), e.status, e.lease_token, e.lease_expires_at, e.started_at, coalesce(e.reply_message_sequence, 0)"

func scanAgentExecution(row pgx.Row, extra ...any) (AgentExecution, error) {
	var execution AgentExecution
	var conversationID, investigationID, leaseToken uuid.NullUUID
	fields := []any{&execution.ID, &conversationID, &investigationID,
		&execution.MessageSequence, &execution.Status, &leaseToken, &execution.LeaseExpiresAt, &execution.StartedAt, &execution.ReplyMessageSequence}
	err := row.Scan(append(fields, extra...)...)
	execution.ConversationID, execution.InvestigationID, execution.LeaseToken = conversationID.UUID, investigationID.UUID, leaseToken.UUID
	return execution, err
}

func (p *Database) AdmitAgentChat(
	ctx context.Context, principal authz.Principal, organization, conversationID, clientMessageID uuid.UUID,
	input conversation.NewMessage,
) (AgentExecution, error) {
	if principal.IsEmpty() || !principal.HavePermission(authz.ConversationWrite) {
		return AgentExecution{}, ErrNotAMember
	}
	if clientMessageID == uuid.Nil || strings.TrimSpace(input.Text) == "" {
		return AgentExecution{}, errors.New("client message ID and text are required")
	}

	return audited(ctx, p, principal, organization, audit.ActionConversationMessage,
		func(ctx context.Context, tx pgx.Tx) (AgentExecution, audit.Target, audit.Detail, error) {
			execution, err := admitAgentChat(ctx, tx, principal, organization, conversationID, clientMessageID, input)
			return execution, audit.Target{Kind: audit.TargetConversation, ID: conversationID.String()},
				audit.Detail{"sequence": execution.MessageSequence}, err
		})
}

func admitAgentChat(
	ctx context.Context, tx pgx.Tx, principal authz.Principal, organization, conversationID, clientMessageID uuid.UUID,
	input conversation.NewMessage,
) (AgentExecution, error) {
	if err := lockConversationQueue(ctx, tx, organization); err != nil {
		return AgentExecution{}, err
	}
	var now time.Time
	err := tx.QueryRow(ctx, `
		SELECT now() FROM conversation
		 WHERE org_id = $1 AND conversation_id = $2 AND source = 'web'
		 FOR UPDATE`, organization, conversationID).Scan(&now)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentExecution{}, conversation.ErrUnknown
	}
	if err != nil {
		return AgentExecution{}, fmt.Errorf("locking the Chat Conversation: %w", err)
	}

	lead := conversation.QuestionWindow(conversation.DefaultIncidentWindowLead)
	window := conversation.Window{From: now.Add(-lead), Until: now}
	var from, until *time.Time
	if input.Window != nil {
		window = input.Window.Normalized()
		if !window.Valid(now) {
			return AgentExecution{}, conversation.ErrInvalidWindow
		}
		from, until = &window.From, &window.Until
	}
	authorID := principal.UserID().String()
	var identical bool
	prior, err := scanAgentExecution(tx.QueryRow(ctx, `SELECT `+agentExecutionColumns+`,
	        m.author_id = $4 AND m.text = $5
	        AND m.window_from = coalesce($6::timestamptz, m.created_at - $8::interval)
	        AND m.window_until = coalesce($7::timestamptz, m.created_at)
	   FROM conversation_message m
	   JOIN agent_execution e USING (org_id, conversation_id)
	  WHERE m.org_id = $1 AND m.conversation_id = $2 AND m.client_message_id = $3
	    AND e.message_sequence = m.sequence`,
		organization, conversationID, clientMessageID, authorID, input.Text, from, until, lead.String()), &identical)
	if err == nil {
		if !identical {
			return AgentExecution{}, ErrClientMessageConflict
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentExecution{}, fmt.Errorf("reading accepted Chat input: %w", err)
	}

	if err := checkConversationQueueCapacity(ctx, tx, organization); err != nil {
		return AgentExecution{}, err
	}
	input.Role = conversation.RoleUser
	input.AuthorID = authorID
	input.AuthorName = principal.DisplayName()
	message, err := insertConversationMessage(ctx, tx, organization, conversationID, input, window, &clientMessageID)
	if err != nil {
		return AgentExecution{}, err
	}
	execution, err := scanAgentExecution(tx.QueryRow(ctx, `
		INSERT INTO agent_execution AS e (org_id, execution_id, conversation_id, message_sequence)
		VALUES ($1, $2, $3, $4) RETURNING `+agentExecutionColumns,
		organization, uuid.New(), conversationID, message.Sequence))
	if err != nil {
		return AgentExecution{}, fmt.Errorf("creating Chat execution: %w", err)
	}
	return execution, nil
}

func (p *Database) AdmitAgentInvestigation(ctx context.Context, organization, investigationID uuid.UUID) (AgentExecution, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return AgentExecution{}, err
	}
	execution, err := scanAgentExecution(pool.QueryRow(ctx, `
		INSERT INTO agent_execution AS e (org_id, execution_id, investigation_id)
		SELECT org_id, $3, investigation_id FROM investigation
		 WHERE org_id = $1 AND investigation_id = $2 AND conversation_id IS NULL AND status = 1 AND lease_worker = ''
		 FOR NO KEY UPDATE
		ON CONFLICT (org_id, investigation_id) DO NOTHING
		RETURNING `+agentExecutionColumns, organization, investigationID, uuid.New()))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentExecution{}, ErrAgentExecutionUnavailable
	}
	if err != nil {
		return AgentExecution{}, fmt.Errorf("creating Investigation execution: %w", err)
	}
	return execution, nil
}

const agentExecutionEligible = `
	AND (e.status = 'queued' OR (e.status = 'running' AND e.lease_expires_at <= clock_timestamp()))
	AND (e.investigation_id IS NULL OR EXISTS (SELECT 1 FROM investigation owner
	      WHERE owner.org_id = e.org_id AND owner.investigation_id = e.investigation_id
	        AND owner.status = 1 AND owner.lease_worker = ''))
	AND NOT EXISTS (
	    SELECT 1 FROM agent_execution head
	     WHERE head.org_id = e.org_id AND head.conversation_id = e.conversation_id
	       AND head.message_sequence < e.message_sequence
	       AND head.status IN ('queued', 'running', 'needs_input'))`

// ClaimNextAgentExecution discovers one runnable execution across the host's Organizations.
// All changes use the same Organization-scoped claim and owner lock as explicit claims.
func (p *Database) ClaimNextAgentExecution(ctx context.Context, leaseFor time.Duration) (uuid.UUID, AgentExecution, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT e.org_id, e.execution_id FROM agent_execution e
		WHERE e.status IN ('queued', 'running') `+agentExecutionEligible+`
		ORDER BY e.created_at, e.execution_id LIMIT 16`)
	if err != nil {
		return uuid.Nil, AgentExecution{}, err
	}
	type candidate struct{ organization, id uuid.UUID }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var item candidate
		err := row.Scan(&item.organization, &item.id)
		return item, err
	})
	if err != nil {
		return uuid.Nil, AgentExecution{}, err
	}
	for _, item := range candidates {
		execution, err := p.claimAgentExecution(ctx, item.organization, item.id, leaseFor, true)
		if !errors.Is(err, ErrAgentExecutionUnavailable) {
			return item.organization, execution, err
		}
	}
	return uuid.Nil, AgentExecution{}, ErrAgentExecutionUnavailable
}

func (p *Database) ClaimAgentExecution(ctx context.Context, organization, id uuid.UUID, leaseFor time.Duration) (AgentExecution, error) {
	return p.claimAgentExecution(ctx, organization, id, leaseFor, false)
}

func (p *Database) claimAgentExecution(
	ctx context.Context, organization, id uuid.UUID, leaseFor time.Duration, skipBusy bool,
) (AgentExecution, error) {
	if leaseFor <= 0 {
		return AgentExecution{}, errors.New("agent execution lease must be positive")
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return AgentExecution{}, err
	}
	var execution AgentExecution
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var conversationID, investigationID *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT conversation_id, investigation_id FROM agent_execution
		    WHERE org_id = $1 AND execution_id = $2`, organization, id).Scan(&conversationID, &investigationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentExecutionUnavailable
		}
		if err != nil {
			return err
		}
		if skipBusy {
			// Discovery skips busy owners and keeps the same owner-before-execution lock order.
			var owner int
			if conversationID != nil {
				err = tx.QueryRow(ctx, `SELECT 1 FROM conversation WHERE org_id = $1 AND conversation_id = $2
					FOR UPDATE SKIP LOCKED`, organization, *conversationID).Scan(&owner)
			} else {
				err = tx.QueryRow(ctx, `SELECT 1 FROM investigation WHERE org_id = $1 AND investigation_id = $2
					FOR NO KEY UPDATE SKIP LOCKED`, organization, *investigationID).Scan(&owner)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAgentExecutionUnavailable
			}
			if err != nil {
				return err
			}
		} else if conversationID != nil {
			if err := lockConversation(ctx, tx, organization, *conversationID); err != nil {
				return err
			}
		} else if err := lockInvestigation(ctx, tx, organization, *investigationID); err != nil {
			return err
		}

		execution, err = scanAgentExecution(tx.QueryRow(ctx, `
			UPDATE agent_execution AS e
			   SET status = 'running', lease_token = $3, lease_expires_at = clock_timestamp() + $4::interval,
			       started_at = coalesce(e.started_at, clock_timestamp())
			 WHERE e.org_id = $1 AND e.execution_id = $2
			   `+agentExecutionEligible+`
			RETURNING `+agentExecutionColumns, organization, id, uuid.New(), leaseFor.String()))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentExecutionUnavailable
		}
		return err
	})
	if err != nil {
		return AgentExecution{}, fmt.Errorf("claiming an agent execution: %w", err)
	}
	return execution, nil
}

func (p *Database) ParkAgentExecution(ctx context.Context, org uuid.UUID, execution AgentExecution) error {
	return p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		return endAgentLease(ctx, tx, org, execution, AgentExecutionNeedsInput, 0)
	})
}

func endAgentLease(ctx context.Context, tx pgx.Tx, organization uuid.UUID, execution AgentExecution, status AgentExecutionStatus, replySequence int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE agent_execution SET status = $3, lease_token = NULL, lease_expires_at = NULL, reply_message_sequence = nullif($4::bigint, 0)
		 WHERE org_id = $1 AND execution_id = $2`, organization, execution.ID, status, replySequence); err != nil {
		return err
	}

	switch status {
	case AgentExecutionCompleted:
		_, err := tx.Exec(ctx, `
			DELETE FROM agent_checkpoint c WHERE c.org_id = $1 AND c.session_id = $2
			  AND c.writer_execution_id <> $3 AND EXISTS (
			      SELECT 1 FROM agent_checkpoint current
			       WHERE current.org_id = $1 AND current.session_id = $2 AND current.writer_execution_id = $3)`,
			organization, execution.SessionID(), execution.ID)
		return err
	case AgentExecutionFailed:
		_, err := tx.Exec(ctx, `DELETE FROM agent_checkpoint WHERE org_id = $1 AND writer_execution_id = $2`,
			organization, execution.ID)
		return err
	default:
		return nil
	}
}

func (p *Database) withAgentLease(ctx context.Context, organization uuid.UUID, execution AgentExecution, action func(pgx.Tx) error) error {
	if execution.LeaseToken == uuid.Nil {
		return ErrAgentLeaseLost
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return useAgentLease(ctx, tx, organization, execution, action)
	})
}

func useAgentLease(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution, action func(pgx.Tx) error) error {
	// Lock before checking time so a transaction that waited cannot use an expired lease.
	var expires time.Time
	err := tx.QueryRow(ctx, `
		WITH owned AS MATERIALIZED (
		    SELECT lease_expires_at FROM agent_execution
		     WHERE org_id = $1 AND execution_id = $2 AND status = 'running'
		       AND lease_token = $3 AND conversation_id IS NOT DISTINCT FROM $4::uuid
		       AND investigation_id IS NOT DISTINCT FROM $5::uuid
		       AND coalesce(message_sequence, 0) = $6
		     FOR UPDATE)
		SELECT lease_expires_at FROM owned WHERE lease_expires_at > clock_timestamp()`,
		org, execution.ID, execution.LeaseToken, nullableUUID(execution.ConversationID),
		nullableUUID(execution.InvestigationID), execution.MessageSequence).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentLeaseLost
	}
	if err != nil {
		return err
	}
	if err := action(tx); err != nil {
		return err
	}
	var current bool
	if err := tx.QueryRow(ctx, "SELECT $1::timestamptz > clock_timestamp()", expires).Scan(&current); err != nil {
		return err
	}
	if !current {
		return ErrAgentLeaseLost
	}
	return nil
}

func (p *Database) RenewAgentExecution(ctx context.Context, org uuid.UUID, execution AgentExecution, leaseFor time.Duration) error {
	return p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_execution SET lease_expires_at = clock_timestamp() + $3::interval
			WHERE org_id = $1 AND execution_id = $2`, org, execution.ID, leaseFor.String())
		return err
	})
}

func (p *Database) AgentExecutionInput(ctx context.Context, org uuid.UUID, execution AgentExecution) (string, error) {
	var text string
	err := p.withAgentLease(ctx, org, execution, func(tx pgx.Tx) error {
		if execution.ConversationID != uuid.Nil {
			return tx.QueryRow(ctx, `SELECT text FROM conversation_message
				WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3`,
				org, execution.ConversationID, execution.MessageSequence).Scan(&text)
		}
		return tx.QueryRow(ctx, `SELECT subject FROM investigation WHERE org_id = $1 AND investigation_id = $2`,
			org, execution.InvestigationID).Scan(&text)
	})
	return text, err
}

// FinishAgentExecution commits the product result and the execution status in one fenced transaction.
func (p *Database) FinishAgentExecution(ctx context.Context, org uuid.UUID, execution AgentExecution,
	status AgentExecutionStatus, reply string, conclusion *investigation.Conclusion, usage investigation.Usage,
) error {
	if status != AgentExecutionFailed && status != AgentExecutionCompleted {
		return errors.New("invalid agent terminal outcome")
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if execution.ConversationID != uuid.Nil {
			err = lockConversation(ctx, tx, org, execution.ConversationID)
		} else {
			err = lockInvestigation(ctx, tx, org, execution.InvestigationID)
		}
		if errors.Is(err, conversation.ErrUnknown) || errors.Is(err, investigation.ErrUnknown) {
			return ErrAgentLeaseLost
		}
		if err != nil {
			return err
		}
		return useAgentLease(ctx, tx, org, execution, func(tx pgx.Tx) error {
			var sequence int64
			if execution.ConversationID != uuid.Nil {
				if status == AgentExecutionCompleted {
					var err error
					sequence, err = insertAgentReply(ctx, tx, org, execution, reply)
					if err != nil {
						return err
					}
				}
			} else if err := finishAgentInvestigation(ctx, tx, org, execution, status, conclusion, usage); err != nil {
				return err
			}
			return endAgentLease(ctx, tx, org, execution, status, sequence)
		})
	})
}

func insertAgentReply(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution, text string) (int64, error) {
	if strings.TrimSpace(text) == "" {
		return 0, errors.New("completed Chat execution has no reply")
	}
	if utf8.RuneCountInString(text) > conversation.MaxMessageTextLength {
		return 0, errors.New("completed Chat reply exceeds the Message limit")
	}
	var window conversation.Window
	err := tx.QueryRow(ctx, `SELECT window_from, window_until FROM conversation_message
		WHERE org_id = $1 AND conversation_id = $2 AND sequence = $3`,
		org, execution.ConversationID, execution.MessageSequence).Scan(&window.From, &window.Until)
	if err != nil {
		return 0, err
	}
	written, err := insertConversationMessage(ctx, tx, org, execution.ConversationID,
		conversation.NewMessage{Role: conversation.RoleAssistant, Text: text}, window, nil)
	return written.Sequence, err
}

func finishAgentInvestigation(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution,
	status AgentExecutionStatus, conclusion *investigation.Conclusion, usage investigation.Usage,
) error {
	ended := investigation.StatusFailed
	reason := "agent reply failed"
	var payload investigation.EventPayload = investigation.FailedPayload(reason)
	result := []byte("{}")
	if status == AgentExecutionCompleted {
		if conclusion == nil {
			return errors.New("completed Investigation requires a validated conclusion")
		}
		encoded, err := json.Marshal(conclusion)
		if err != nil {
			return err
		}
		ended, reason, result = investigation.StatusConcluded, "", encoded
		payload = investigation.ConcludedPayload(*conclusion, "")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE investigation SET status = $3, conclusion = $4::jsonb, error = $5, concluded_at = now(),
		       spend_input_tokens = $6, spend_output_tokens = $7
		 WHERE org_id = $1 AND investigation_id = $2 AND status = $8 AND lease_worker = ''`,
		org, execution.InvestigationID, int16(ended), result, reason, usage.InputTokens, usage.OutputTokens,
		int16(investigation.StatusRunning))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrAgentLeaseLost
	}
	return appendInvestigationTerminalEvent(ctx, tx, org, execution.InvestigationID, payload)
}

func (p *Database) CancelAgentChatExecution(ctx context.Context, org, conversationID, id uuid.UUID) error {
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		found, err := revokeAgentExecution(ctx, tx, org, conversationID, id, uuid.Nil)
		if err == nil && !found {
			return ErrAgentExecutionUnavailable
		}
		return err
	})
}

func revokeAgentExecution(ctx context.Context, tx pgx.Tx, org, conversationID, id, investigationID uuid.UUID) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `
		WITH revoked AS (
		    UPDATE agent_execution SET status = 'cancelled', lease_token = NULL, lease_expires_at = NULL
		     WHERE org_id = $1 AND ((conversation_id = $2 AND execution_id = $3) OR investigation_id = $4)
		       AND status IN ('queued', 'running', 'needs_input')
		     RETURNING execution_id
		), discarded AS (
		    DELETE FROM agent_checkpoint c USING revoked r
		     WHERE c.org_id = $1 AND c.writer_execution_id = r.execution_id
		)
		SELECT EXISTS (SELECT 1 FROM agent_execution WHERE org_id = $1
		    AND ((conversation_id = $2 AND execution_id = $3) OR investigation_id = $4))`,
		org, nullableUUID(conversationID), nullableUUID(id), nullableUUID(investigationID)).Scan(&found)
	return found, err
}

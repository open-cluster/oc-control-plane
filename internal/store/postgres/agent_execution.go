package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
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
	ID              uuid.UUID
	ConversationID  uuid.UUID
	InvestigationID uuid.UUID
	MessageSequence int64
	Status          AgentExecutionStatus
	LeaseToken      uuid.UUID
	LeaseExpiresAt  *time.Time
}

func (execution AgentExecution) SessionID() string {
	if execution.ConversationID != uuid.Nil {
		return "conversation/" + execution.ConversationID.String()
	}
	return "execution/" + execution.ID.String()
}

const agentExecutionColumns = "e.execution_id, e.conversation_id, e.investigation_id, " +
	"coalesce(e.message_sequence, 0), e.status, e.lease_token, e.lease_expires_at"

func scanAgentExecution(row pgx.Row, extra ...any) (AgentExecution, error) {
	var execution AgentExecution
	var conversationID, investigationID, leaseToken uuid.NullUUID
	fields := []any{&execution.ID, &conversationID, &investigationID,
		&execution.MessageSequence, &execution.Status, &leaseToken, &execution.LeaseExpiresAt}
	err := row.Scan(append(fields, extra...)...)
	execution.ConversationID, execution.InvestigationID, execution.LeaseToken = conversationID.UUID, investigationID.UUID, leaseToken.UUID
	return execution, err
}

func (p *Database) AdmitAgentChat(
	ctx context.Context, principal authz.Principal, organization, conversationID, clientMessageID uuid.UUID,
	input conversation.NewMessage,
) (AgentExecution, error) {
	if principal.IsEmpty() || principal.Organization() != organization || !principal.HavePermission(authz.ConversationWrite) {
		return AgentExecution{}, ErrNotAMember
	}
	if clientMessageID == uuid.Nil || strings.TrimSpace(input.Text) == "" {
		return AgentExecution{}, errors.New("client message ID and text are required")
	}

	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return AgentExecution{}, err
	}
	var execution AgentExecution
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		execution, err = admitAgentChat(ctx, tx, principal, organization, conversationID, clientMessageID, input)
		return err
	})
	if err != nil {
		return AgentExecution{}, err
	}
	return execution, nil
}

func admitAgentChat(
	ctx context.Context, tx pgx.Tx, principal authz.Principal, organization, conversationID, clientMessageID uuid.UUID,
	input conversation.NewMessage,
) (AgentExecution, error) {
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
	if err := writeEvent(ctx, tx, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionConversationMessage,
		Target:        audit.Target{Kind: audit.TargetConversation, ID: conversationID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
		Detail:        audit.Detail{"sequence": execution.MessageSequence},
	}); err != nil {
		return AgentExecution{}, err
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
		SELECT org_id, $3, investigation_id FROM investigation WHERE org_id = $1 AND investigation_id = $2
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

func (p *Database) ClaimAgentExecution(
	ctx context.Context, organization, id uuid.UUID, leaseFor time.Duration,
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
		var conversationID *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT conversation_id FROM agent_execution
		    WHERE org_id = $1 AND execution_id = $2`, organization, id).Scan(&conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentExecutionUnavailable
		}
		if err != nil {
			return err
		}
		if conversationID != nil {
			if err := lockConversation(ctx, tx, organization, *conversationID); err != nil {
				return err
			}
		}

		execution, err = scanAgentExecution(tx.QueryRow(ctx, `
			UPDATE agent_execution AS e
			   SET status = 'running', lease_token = $3, lease_expires_at = clock_timestamp() + $4::interval
			 WHERE e.org_id = $1 AND e.execution_id = $2
			   AND (e.status = 'queued' OR (e.status = 'running' AND e.lease_expires_at <= clock_timestamp()))
			   AND NOT EXISTS (
			       SELECT 1 FROM agent_execution head
			        WHERE head.org_id = e.org_id AND head.conversation_id = e.conversation_id
			          AND head.message_sequence < e.message_sequence
			          AND head.status IN ('queued', 'running', 'needs_input'))
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

func (p *Database) EndAgentExecution(ctx context.Context, organization uuid.UUID, execution AgentExecution, status AgentExecutionStatus) error {
	switch status {
	case AgentExecutionNeedsInput, AgentExecutionCompleted, AgentExecutionFailed, AgentExecutionCancelled:
	default:
		return errors.New("invalid status for ending an agent lease")
	}
	return p.withAgentLease(ctx, organization, execution, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE agent_execution
			   SET status = $3, lease_token = NULL, lease_expires_at = NULL
			 WHERE org_id = $1 AND execution_id = $2`, organization, execution.ID, status)
		if err != nil {
			return err
		}
		return finishAgentCheckpoint(ctx, tx, organization, execution, status)
	})
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
		// Lock before checking time so a transaction that waited cannot use an expired lease.
		var expires time.Time
		err := tx.QueryRow(ctx, `
			WITH owned AS MATERIALIZED (
			    SELECT lease_expires_at FROM agent_execution
			     WHERE org_id = $1 AND execution_id = $2 AND status = 'running'
			       AND lease_token = $3 AND conversation_id IS NOT DISTINCT FROM $4::uuid
			     FOR UPDATE)
			SELECT lease_expires_at FROM owned WHERE lease_expires_at > clock_timestamp()`,
			organization, execution.ID, execution.LeaseToken, nullableUUID(execution.ConversationID)).Scan(&expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentLeaseLost
		}
		if err != nil {
			return err
		}
		if err := action(tx); err != nil {
			return err
		}
		// A checkpoint statement can also wait on its own row. Roll it back if its lease lapsed.
		var current bool
		if err := tx.QueryRow(ctx, "SELECT $1::timestamptz > clock_timestamp()", expires).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrAgentLeaseLost
		}
		return nil
	})
}

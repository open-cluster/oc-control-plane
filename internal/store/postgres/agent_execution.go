package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
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
	ErrAgentEvidenceUnavailable  = errors.New("unavailable Tool Run evidence")
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
	ID                          uuid.UUID
	ConversationID              uuid.UUID
	InvestigationID             uuid.UUID
	MessageSequence             int64
	ReplyMessageSequence        int64
	Status                      AgentExecutionStatus
	LeaseToken                  uuid.UUID
	LeaseExpiresAt              *time.Time
	StartedAt                   *time.Time
	investigationConversationID uuid.UUID
}

func (execution AgentExecution) SessionID() string {
	if id := execution.checkpointConversation(); id != uuid.Nil {
		return "conversation/" + id.String()
	}
	return "execution/" + execution.ID.String()
}

func (execution AgentExecution) checkpointConversation() uuid.UUID {
	if execution.ConversationID != uuid.Nil {
		return execution.ConversationID
	}
	return execution.investigationConversationID
}

const agentExecutionColumns = "e.execution_id, e.conversation_id, e.investigation_id, " +
	"coalesce(e.message_sequence, 0), e.status, e.lease_token, e.lease_expires_at, e.started_at, coalesce(e.reply_message_sequence, 0), " +
	"(SELECT i.conversation_id FROM investigation i WHERE i.org_id = e.org_id AND i.investigation_id = e.investigation_id)"

func scanAgentExecution(row pgx.Row, extra ...any) (AgentExecution, error) {
	var execution AgentExecution
	var conversationID, investigationID, investigationConversation, leaseToken uuid.NullUUID
	fields := []any{&execution.ID, &conversationID, &investigationID,
		&execution.MessageSequence, &execution.Status, &leaseToken, &execution.LeaseExpiresAt, &execution.StartedAt,
		&execution.ReplyMessageSequence, &investigationConversation}
	err := row.Scan(append(fields, extra...)...)
	execution.ConversationID, execution.InvestigationID, execution.LeaseToken = conversationID.UUID, investigationID.UUID, leaseToken.UUID
	execution.investigationConversationID = investigationConversation.UUID
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

func queueInvestigation(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_execution (org_id, execution_id, investigation_id)
		VALUES ($1, gen_random_uuid(), $2)`, org, id)
	return err
}

const agentExecutionEligible = `
	AND (e.status = 'queued' OR (e.status = 'running' AND e.lease_expires_at <= clock_timestamp()))
	AND (e.investigation_id IS NULL OR EXISTS (SELECT 1 FROM investigation owner
	      WHERE owner.org_id = e.org_id AND owner.investigation_id = e.investigation_id
	        AND owner.status = 1))
	AND NOT EXISTS (
	    SELECT 1 FROM agent_execution head
	     WHERE head.org_id = e.org_id AND head.conversation_id = e.conversation_id
	       AND head.message_sequence < e.message_sequence
	       AND head.status IN ('queued', 'running', 'needs_input'))`

// ClaimNextAgentExecution discovers one runnable execution across the host's Organizations.
// The owner locks serialize claims with terminal writes and permit checkpoint foreign-key checks.
func (p *Database) ClaimNextAgentExecution(ctx context.Context, leaseFor time.Duration) (uuid.UUID, AgentExecution, error) {
	if leaseFor <= 0 {
		return uuid.Nil, AgentExecution{}, errors.New("agent execution lease must be positive")
	}
	var organization, id uuid.UUID
	var execution AgentExecution
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			WITH chat AS MATERIALIZED (
			    SELECT e.org_id, e.execution_id, e.created_at FROM agent_execution e
			      JOIN conversation c ON c.org_id = e.org_id AND c.conversation_id = e.conversation_id
			     WHERE e.status IN ('queued', 'running') `+agentExecutionEligible+`
			     ORDER BY e.created_at, e.execution_id LIMIT 1 FOR NO KEY UPDATE OF c SKIP LOCKED
			), investigation_work AS MATERIALIZED (
			    SELECT e.org_id, e.execution_id, e.created_at FROM agent_execution e
			      JOIN investigation i ON i.org_id = e.org_id AND i.investigation_id = e.investigation_id
			     WHERE e.status IN ('queued', 'running') `+agentExecutionEligible+`
			     ORDER BY e.created_at, e.execution_id LIMIT 1 FOR NO KEY UPDATE OF i SKIP LOCKED
			) SELECT org_id, execution_id FROM (
				SELECT * FROM chat UNION ALL SELECT * FROM investigation_work
			) AS candidates ORDER BY created_at, execution_id LIMIT 1`).Scan(&organization, &id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentExecutionUnavailable
		}
		if err != nil {
			return err
		}
		execution, err = claimEligibleAgentExecution(ctx, tx, organization, id, leaseFor)
		return err
	})
	if err != nil {
		return uuid.Nil, AgentExecution{}, fmt.Errorf("claiming the next agent execution: %w", err)
	}
	return organization, execution, nil
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
		var conversationID, investigationID *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT conversation_id, investigation_id FROM agent_execution
		    WHERE org_id = $1 AND execution_id = $2`, organization, id).Scan(&conversationID, &investigationID)
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
		} else if err := lockInvestigation(ctx, tx, organization, *investigationID); err != nil {
			return err
		}
		execution, err = claimEligibleAgentExecution(ctx, tx, organization, id, leaseFor)
		return err
	})
	if err != nil {
		return AgentExecution{}, fmt.Errorf("claiming an agent execution: %w", err)
	}
	return execution, nil
}

func claimEligibleAgentExecution(
	ctx context.Context, tx pgx.Tx, organization, id uuid.UUID, leaseFor time.Duration,
) (AgentExecution, error) {
	execution, err := scanAgentExecution(tx.QueryRow(ctx, `
		UPDATE agent_execution AS e
		   SET status = 'running', lease_token = $3, lease_expires_at = clock_timestamp() + $4::interval,
		       started_at = coalesce(e.started_at, clock_timestamp())
		 WHERE e.org_id = $1 AND e.execution_id = $2
		   `+agentExecutionEligible+`
		RETURNING `+agentExecutionColumns, organization, id, uuid.New(), leaseFor.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentExecution{}, ErrAgentExecutionUnavailable
	}
	if err != nil {
		return AgentExecution{}, err
	}
	if err := interruptAgentToolRuns(ctx, tx, organization, id); err != nil {
		return AgentExecution{}, err
	}
	if execution.InvestigationID != uuid.Nil {
		_, err = tx.Exec(ctx, `INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
			SELECT $1, $2, 1, $3, jsonb_build_object('text', $4::text)
			WHERE NOT EXISTS (SELECT 1 FROM investigation_event WHERE org_id = $1 AND investigation_id = $2)`,
			organization, execution.InvestigationID, int16(investigation.EventStarted),
			"Reviewing this request and available evidence sources.")
	}
	return execution, err
}

func endAgentLease(ctx context.Context, tx pgx.Tx, organization uuid.UUID, execution AgentExecution, status AgentExecutionStatus, replySequence int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE agent_execution SET status = $3, lease_token = NULL, lease_expires_at = NULL, reply_message_sequence = nullif($4::bigint, 0)
		 WHERE org_id = $1 AND execution_id = $2`, organization, execution.ID, status, replySequence); err != nil {
		return err
	}

	if err := interruptAgentToolRuns(ctx, tx, organization, execution.ID); err != nil {
		return err
	}
	return finishAgentCheckpoint(ctx, tx, organization, execution, status)
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
	return useExecutionLease(ctx, tx, org, execution.ID, execution.LeaseToken, func(owned AgentExecution) error {
		if owned.ConversationID != execution.ConversationID || owned.InvestigationID != execution.InvestigationID ||
			owned.MessageSequence != execution.MessageSequence || owned.investigationConversationID != execution.investigationConversationID {
			return ErrAgentLeaseLost
		}
		return action(tx)
	})
}

func useExecutionLease(
	ctx context.Context, tx pgx.Tx, org, id, token uuid.UUID, action func(AgentExecution) error,
) error {
	if token == uuid.Nil {
		return ErrAgentLeaseLost
	}
	// Lock before checking time so a transaction that waited cannot use an expired lease.
	var expires time.Time
	var owned AgentExecution
	var conversationID, investigationID, investigationConversation uuid.NullUUID
	err := tx.QueryRow(ctx, `
		WITH owned AS MATERIALIZED (
		    SELECT lease_expires_at, conversation_id, investigation_id, coalesce(message_sequence, 0) AS message_sequence
		     FROM agent_execution
		     WHERE org_id = $1 AND execution_id = $2 AND status = 'running' AND lease_token = $3
		     FOR UPDATE)
		SELECT owned.lease_expires_at, owned.conversation_id, owned.investigation_id, owned.message_sequence, i.conversation_id
		  FROM owned LEFT JOIN investigation i ON i.org_id = $1 AND i.investigation_id = owned.investigation_id
		 WHERE owned.lease_expires_at > clock_timestamp()`,
		org, id, token).Scan(&expires, &conversationID, &investigationID, &owned.MessageSequence, &investigationConversation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentLeaseLost
	}
	if err != nil {
		return err
	}
	owned.ConversationID, owned.InvestigationID = conversationID.UUID, investigationID.UUID
	owned.investigationConversationID = investigationConversation.UUID
	if err := action(owned); err != nil {
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

// FinishAgentExecution commits the product result and any final native checkpoint in one fenced transaction.
func (p *Database) FinishAgentExecution(ctx context.Context, org uuid.UUID, execution AgentExecution,
	status AgentExecutionStatus, reply string, conclusion *investigation.Conclusion, usage investigation.Usage,
	checkpoint ...*agent.AgentState,
) error {
	if status != AgentExecutionFailed && status != AgentExecutionCompleted {
		return errors.New("invalid agent terminal outcome")
	}
	var state []byte
	if len(checkpoint) > 0 {
		if len(checkpoint) != 1 || status != AgentExecutionCompleted {
			return ErrAgentCheckpoint
		}
		var err error
		state, err = encodeAgentCheckpoint(execution.SessionID(), checkpoint[0])
		if err != nil {
			return err
		}
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		conversationID := execution.checkpointConversation()
		var err error
		if conversationID != uuid.Nil {
			err = lockConversation(ctx, tx, org, conversationID)
		}
		if err == nil && execution.InvestigationID != uuid.Nil {
			err = lockInvestigation(ctx, tx, org, execution.InvestigationID)
		}
		if errors.Is(err, conversation.ErrUnknown) || errors.Is(err, investigation.ErrUnknown) {
			return ErrAgentLeaseLost
		}
		if err != nil {
			return err
		}
		return useAgentLease(ctx, tx, org, execution, func(tx pgx.Tx) error {
			if state != nil {
				if err := writeAgentCheckpoint(ctx, tx, org, execution, state); err != nil {
					return err
				}
			}
			var sequence int64
			if execution.ConversationID != uuid.Nil {
				if status == AgentExecutionCompleted {
					var err error
					sequence, err = insertAgentReply(ctx, tx, org, execution, reply)
					if err != nil {
						return err
					}
				}
			} else {
				if err := finishAgentInvestigation(ctx, tx, org, execution, status, conclusion, usage); err != nil {
					return err
				}
				if conversationID != uuid.Nil && status == AgentExecutionCompleted {
					if err := insertInvestigationReply(ctx, tx, org, conversationID, execution.InvestigationID, reply); err != nil {
						return err
					}
				}
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
		if err := validateAgentConclusionEvidence(ctx, tx, org, execution, *conclusion); err != nil {
			return err
		}
		if err := investigation.ValidateConclusionStatus(*conclusion); err != nil {
			return err
		}
		if _, _, err := agentToolScope(ctx, tx, org, execution); err != nil {
			return err
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
		 WHERE org_id = $1 AND investigation_id = $2 AND status = $8`,
		org, execution.InvestigationID, int16(ended), result, reason, usage.InputTokens, usage.OutputTokens,
		int16(investigation.StatusRunning))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrAgentLeaseLost
	}
	return appendInvestigationEvent(ctx, tx, org, execution.InvestigationID, payload)
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
	var revoked uuid.NullUUID
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
		SELECT (SELECT execution_id FROM revoked), EXISTS (SELECT 1 FROM agent_execution WHERE org_id = $1
		    AND ((conversation_id = $2 AND execution_id = $3) OR investigation_id = $4))`,
		org, nullableUUID(conversationID), nullableUUID(id), nullableUUID(investigationID)).Scan(&revoked, &found)
	if err == nil && revoked.Valid {
		err = interruptAgentToolRuns(ctx, tx, org, revoked.UUID)
	}
	return found, err
}

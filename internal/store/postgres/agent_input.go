package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/execution"
)

type agentInput struct {
	execution.PendingInput
	ReplyID      string                `json:"replyId"`
	ToolCallID   string                `json:"toolCallId"`
	Iteration    int                   `json:"iteration"`
	WaitingSince *time.Time            `json:"waitingSince,omitempty"`
	ParkedMicros int64                 `json:"parkedMicros"`
	Answer       *tool.AskUserMetadata `json:"answer,omitempty"`
	AnswerDigest string                `json:"answerDigest,omitempty"`
}

func (input *agentInput) result() (*message.ToolResultBlock, error) {
	body, err := json.Marshal(input.Answer)
	if err != nil {
		return nil, err
	}
	return &message.ToolResultBlock{
		Type: "tool_result", ID: input.ToolCallID, Name: "AskUser", State: message.ToolResultSuccess,
		Output:   "Operator answer (untrusted): " + string(body),
		Metadata: map[string]any{"answers": input.Answer.Answers},
	}, nil
}

// ParkAskUser publishes a saved native wait or returns the answer for the exact
// restored wait. A nil result means the caller's lease has been revoked.
func (s *AgentStateSaver) ParkAskUser(ctx context.Context, replyID string, call message.ToolCallBlock,
	questions []tool.AskUserQuestion,
) (*message.ToolResultBlock, error) {
	var answer *message.ToolResultBlock
	err := s.database.withAgentLease(ctx, s.organization, s.execution, func(tx pgx.Tx) error {
		if err := authorizeAskUserExecution(ctx, tx, s.organization, s.execution); err != nil {
			return err
		}
		var state agent.AgentState
		err := tx.QueryRow(ctx, `
			SELECT state FROM agent_checkpoint
			 WHERE org_id = $1 AND session_id = $2 AND writer_execution_id = $3`,
			s.organization, s.execution.SessionID(), s.execution.ID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentCheckpoint
		}
		if err != nil {
			return err
		}
		saved, result := checkpointAskUser(&state, replyID, call.ID)
		if saved.State != message.ToolCallSubmitted || saved.Input != call.Input || result != nil ||
			state.ReplyRecovery == nil || !state.ReplyRecovery.Active || state.ReplyRecovery.ReplyID != replyID {
			return ErrAgentCheckpoint
		}
		iteration := state.ReplyRecovery.NextIteration
		var current *agentInput
		if err := tx.QueryRow(ctx, `SELECT pending_input FROM agent_execution
			WHERE org_id = $1 AND execution_id = $2`, s.organization, s.execution.ID).Scan(&current); err != nil {
			return err
		}
		if current != nil && current.Answer != nil {
			if current.ReplyID != replyID || current.ToolCallID != call.ID || current.Iteration != iteration {
				return ErrAgentCheckpoint
			}
			answer, err = current.result()
			return err
		}
		waiting := agentInput{
			PendingInput: execution.PendingInput{OccurrenceID: uuid.New(), Questions: questions},
			ReplyID:      replyID, ToolCallID: call.ID, Iteration: iteration,
			ParkedMicros: s.execution.ParkedDuration().Microseconds(),
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_execution
			SET pending_input = $3::jsonb || jsonb_build_object('waitingSince', clock_timestamp())
			WHERE org_id = $1 AND execution_id = $2`, s.organization, s.execution.ID, waiting); err != nil {
			return err
		}
		return endAgentLease(ctx, tx, s.organization, s.execution, AgentExecutionNeedsInput, 0)
	})
	return answer, err
}

func (s *AgentStateSaver) ValidateAskUserResume(state *agent.AgentState) error {
	input := s.execution.pendingInput
	if input == nil || input.Answer == nil {
		return nil
	}
	call, result := checkpointAskUser(state, input.ReplyID, input.ToolCallID)
	if call.State != message.ToolCallSubmitted || result != nil || state.ReplyRecovery == nil ||
		!state.ReplyRecovery.Active || state.ReplyRecovery.ReplyID != input.ReplyID ||
		state.ReplyRecovery.NextIteration != input.Iteration {
		return ErrAgentCheckpoint
	}
	return nil
}

func (s *AgentStateSaver) acknowledgeAskUser(ctx context.Context, tx pgx.Tx, state *agent.AgentState) (bool, error) {
	input := s.execution.pendingInput
	if input == nil || input.Answer == nil {
		return false, nil
	}
	call, result := checkpointAskUser(state, input.ReplyID, input.ToolCallID)
	if call.State != message.ToolCallFinished || result == nil {
		return false, nil
	}
	expected, err := input.result()
	if err != nil {
		return false, err
	}
	if result.State != message.ToolResultSuccess || result.GetOutputText() != expected.GetOutputText() {
		return false, ErrAgentCheckpoint
	}
	updated, err := tx.Exec(ctx, `UPDATE agent_execution
		SET pending_input = pending_input - ARRAY['answer', 'questions', 'replyId', 'toolCallId', 'iteration']
		WHERE org_id = $1 AND execution_id = $2 AND pending_input ->> 'occurrenceId' = $3
		  AND pending_input ->> 'answerDigest' = $4 AND pending_input ? 'answer'`,
		s.organization, s.execution.ID, input.OccurrenceID.String(), input.AnswerDigest)
	if err != nil {
		return false, err
	}
	if updated.RowsAffected() != 1 {
		return false, ErrAgentCheckpoint
	}
	if err := authorizeAskUserExecution(ctx, tx, s.organization, s.execution); err != nil {
		return false, err
	}
	return true, nil
}

func checkpointAskUser(state *agent.AgentState, replyID, callID string) (message.ToolCallBlock, *message.ToolResultBlock) {
	if state == nil || state.ReplyID != replyID {
		return message.ToolCallBlock{}, nil
	}
	var call message.ToolCallBlock
	var result *message.ToolResultBlock
	for _, msg := range state.Context {
		if msg == nil || msg.Role != message.RoleAssistant || msg.ID != replyID {
			continue
		}
		for _, block := range msg.Content {
			switch block := block.(type) {
			case message.ToolCallBlock:
				if block.ID == callID && strings.EqualFold(block.Name, "AskUser") {
					if call.ID != "" {
						return message.ToolCallBlock{}, nil
					}
					call = block
				}
			case message.ToolResultBlock:
				if call.ID != "" && block.ID == callID && strings.EqualFold(block.Name, "AskUser") {
					result = &block
				}
			}
		}
	}
	return call, result
}

func authorizeAskUserExecution(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution) error {
	if execution.ConversationID == uuid.Nil {
		return ErrAgentCheckpoint
	}
	_, origin, err := agentToolScope(ctx, tx, org, execution)
	if err != nil {
		return err
	}
	if origin != nil {
		return ErrAgentCheckpoint
	}
	return nil
}

func (p *Database) AnswerChatInput(
	ctx context.Context, principal authz.Principal, org, conversationID, executionID, occurrenceID uuid.UUID,
	answer tool.AskUserMetadata,
) error {
	if principal.Organization() != org || !principal.HavePermission(authz.ConversationWrite) {
		return authz.ErrNotAMember
	}
	pool, err := p.poolForOrganization(org)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Membership mutations lock the Organization before locking Membership rows.
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `SELECT org_id FROM organization WHERE org_id = $1 FOR KEY SHARE`, org).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotAMember
		}
		if err != nil {
			return err
		}
		if err := lockConversation(ctx, tx, org, conversationID); err != nil {
			return err
		}
		var status string
		var input *agentInput
		err = tx.QueryRow(ctx, `
			SELECT e.status, e.pending_input FROM agent_execution e
			JOIN conversation c USING (org_id, conversation_id)
			JOIN conversation_message m ON m.org_id = e.org_id AND m.conversation_id = e.conversation_id
			    AND m.sequence = e.message_sequence
			WHERE e.org_id = $1 AND e.conversation_id = $2 AND e.execution_id = $3
			  AND c.source = 'web' AND c.deleted_at IS NULL AND m.role = $4 AND m.author_id = $5
			FOR UPDATE OF e`, org, conversationID, executionID, int16(conversation.RoleUser), principal.UserID().String()).
			Scan(&status, &input)
		if errors.Is(err, pgx.ErrNoRows) {
			return conversation.ErrUnknown
		}
		if err != nil {
			return err
		}
		_, maximum := session.Lifetimes()
		var expires time.Time
		err = tx.QueryRow(ctx, `SELECT LEAST(expires_at, created_at + $3::interval)
			FROM session WHERE session_id = $1 AND user_id = $2 FOR SHARE`,
			principal.SessionID(), principal.UserID(), maximum).Scan(&expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotAMember
		}
		if err != nil {
			return err
		}
		if err := authorizeAgentActors(ctx, tx, org, authz.ConversationWrite, []string{principal.UserID().String()}); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return err
		}
		if !now.Before(expires) {
			return authz.ErrNotAMember
		}
		if input == nil || input.OccurrenceID != occurrenceID {
			return conversation.ErrInputConflict
		}
		body, err := json.Marshal(answer)
		if err != nil {
			return err
		}
		for _, item := range answer.Answers {
			if strings.ContainsRune(item.Other, 0) || utf8.RuneCountInString(item.Other) > conversation.MaxMessageTextLength {
				return conversation.ErrInvalidInputAnswer
			}
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		if input.AnswerDigest != "" {
			if input.AnswerDigest == digest {
				return nil
			}
			return conversation.ErrInputConflict
		}
		if status != string(AgentExecutionNeedsInput) || input.WaitingSince == nil {
			return conversation.ErrInputConflict
		}
		input.Answer, input.AnswerDigest = &answer, digest
		result, err := input.result()
		if err != nil {
			return err
		}
		validator := tool.AskUserTool().(tool.ExternalResultValidator)
		if err := validator.ValidateExternalResult(map[string]any{"questions": input.Questions}, result); err != nil {
			return conversation.ErrInvalidInputAnswer
		}
		input.ParkedMicros += max(0, now.Sub(*input.WaitingSince).Microseconds())
		input.WaitingSince = nil
		if _, err := tx.Exec(ctx, `UPDATE agent_execution SET status = 'queued', pending_input = $3::jsonb
			WHERE org_id = $1 AND execution_id = $2`, org, executionID, input); err != nil {
			return err
		}
		return writeEvent(ctx, tx, audit.Event{
			Organization: org.String(), Actor: principal.Actor(), Action: audit.ActionChatInputAnswered,
			Target: audit.Target{Kind: audit.TargetConversation, ID: conversationID.String()}, Outcome: audit.OutcomeAllowed,
			SourceAddress: principal.SourceAddress(), RequestID: principal.RequestID(),
			Detail: audit.Detail{"executionId": executionID.String(), "occurrenceId": occurrenceID.String()},
		})
	})
}

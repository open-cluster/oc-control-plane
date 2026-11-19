package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrAgentCheckpoint = errors.New("agent checkpoint is invalid")

type AgentStateSaver struct {
	organization uuid.UUID
	database     *Database
	execution    AgentExecution
}

var _ agent.StateSaver = (*AgentStateSaver)(nil)

func (p *Database) AgentStateSaver(organization uuid.UUID, execution AgentExecution) *AgentStateSaver {
	return &AgentStateSaver{database: p, organization: organization, execution: execution}
}

func (s *AgentStateSaver) SaveState(ctx context.Context, session string, state *agent.AgentState) error {
	if session != s.execution.SessionID() || !validAgentCheckpoint(session, state) {
		return ErrAgentCheckpoint
	}
	checkpoint, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("%w: cannot encode native state", ErrAgentCheckpoint)
	}
	return s.database.withAgentLease(ctx, s.organization, s.execution, func(tx pgx.Tx) error {
		written, err := tx.Exec(ctx, `
			INSERT INTO agent_checkpoint AS c (org_id, session_id, writer_execution_id, conversation_id, state)
			VALUES ($1, $2, $3, $4, $5::jsonb)
			ON CONFLICT (org_id, session_id, committed)
			DO UPDATE SET state = excluded.state
			 WHERE c.writer_execution_id = excluded.writer_execution_id`,
			s.organization, session, s.execution.ID, nullableUUID(s.execution.ConversationID), checkpoint)
		if err == nil && written.RowsAffected() != 1 {
			return ErrAgentLeaseLost
		}
		return err
	})
}

func (s *AgentStateSaver) LoadState(ctx context.Context, session string) (*agent.AgentState, error) {
	if session != s.execution.SessionID() {
		return nil, ErrAgentCheckpoint
	}
	state, _, err := s.Restore(ctx)
	return state, err
}

// Restore returns the current writer, or uuid.Nil for committed Conversation context.
func (s *AgentStateSaver) Restore(ctx context.Context) (*agent.AgentState, uuid.UUID, error) {
	session := s.execution.SessionID()
	var state *agent.AgentState
	var writer uuid.NullUUID
	err := s.database.withAgentLease(ctx, s.organization, s.execution, func(tx pgx.Tx) error {
		var checkpoint []byte
		err := tx.QueryRow(ctx, `
			SELECT state, writer_execution_id FROM agent_checkpoint
			 WHERE org_id = $1 AND session_id = $2
			   AND (writer_execution_id = $3 OR (writer_execution_id IS NULL AND conversation_id = $4))
			 ORDER BY writer_execution_id NULLS LAST LIMIT 1`,
			s.organization, session, s.execution.ID, nullableUUID(s.execution.ConversationID)).Scan(&checkpoint, &writer)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if json.Unmarshal(checkpoint, &state) != nil || !validAgentCheckpoint(session, state) {
			return ErrAgentCheckpoint
		}
		return nil
	})
	if err != nil {
		return nil, uuid.Nil, err
	}
	return state, writer.UUID, nil
}

func (s *AgentStateSaver) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	state, _, err := s.Restore(ctx)
	if err != nil || state == nil {
		return nil, err
	}
	return []agent.SessionInfo{{SessionID: state.SessionID, Summary: state.Summary}}, nil
}

func (s *AgentStateSaver) DeleteSession(ctx context.Context, session string) error {
	if session != s.execution.SessionID() {
		return ErrAgentCheckpoint
	}
	return s.database.withAgentLease(ctx, s.organization, s.execution, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM agent_checkpoint WHERE org_id = $1 AND session_id = $2 AND writer_execution_id = $3`,
			s.organization, session, s.execution.ID)
		return err
	})
}

func finishAgentCheckpoint(ctx context.Context, tx pgx.Tx, org uuid.UUID, execution AgentExecution, status AgentExecutionStatus) error {
	switch status {
	case AgentExecutionCompleted:
		if execution.ConversationID == uuid.Nil {
			return nil
		}
		// Promotion and removal of the previous Conversation snapshot commit with the outcome.
		if _, err := tx.Exec(ctx, `
			DELETE FROM agent_checkpoint WHERE org_id = $1 AND conversation_id = $2
			  AND writer_execution_id IS NULL AND EXISTS (
			      SELECT 1 FROM agent_checkpoint WHERE org_id = $1 AND session_id = $3 AND writer_execution_id = $4)`,
			org, execution.ConversationID, execution.SessionID(), execution.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE agent_checkpoint SET writer_execution_id = NULL, committed = true
			WHERE org_id = $1 AND session_id = $2 AND writer_execution_id = $3`,
			org, execution.SessionID(), execution.ID)
		return err
	case AgentExecutionFailed, AgentExecutionCancelled:
		_, err := tx.Exec(ctx, `DELETE FROM agent_checkpoint WHERE org_id = $1 AND writer_execution_id = $2`, org, execution.ID)
		return err
	default:
		return nil
	}
}

func validAgentCheckpoint(session string, state *agent.AgentState) bool {
	return state != nil && state.SessionID == session &&
		state.SchemaVersion > 0 && state.SchemaVersion <= agent.StateSchemaVersion
}

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
		_, err := tx.Exec(ctx, `
			INSERT INTO agent_checkpoint (org_id, session_id, writer_execution_id, state)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (org_id, session_id) DO UPDATE
			  SET writer_execution_id = excluded.writer_execution_id, state = excluded.state`,
			s.organization, session, s.execution.ID, checkpoint)
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

// Restore identifies the writer so recovery can distinguish a prior Chat turn from this execution.
func (s *AgentStateSaver) Restore(ctx context.Context) (*agent.AgentState, uuid.UUID, error) {
	session := s.execution.SessionID()
	var state *agent.AgentState
	var writer uuid.UUID
	err := s.database.withAgentLease(ctx, s.organization, s.execution, func(tx pgx.Tx) error {
		var checkpoint []byte
		err := tx.QueryRow(ctx, `SELECT state, writer_execution_id FROM agent_checkpoint
		    WHERE org_id = $1 AND session_id = $2`, s.organization, session).Scan(&checkpoint, &writer)
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
	return state, writer, nil
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
		_, err := tx.Exec(ctx, `DELETE FROM agent_checkpoint WHERE org_id = $1 AND session_id = $2`,
			s.organization, session)
		return err
	})
}

func validAgentCheckpoint(session string, state *agent.AgentState) bool {
	return state != nil && state.SessionID == session &&
		state.SchemaVersion > 0 && state.SchemaVersion <= agent.StateSchemaVersion
}

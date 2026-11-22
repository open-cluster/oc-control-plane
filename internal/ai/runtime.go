package ai

import (
	"errors"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Config supplies the profile and host-owned session identity for one agent.
type Config struct {
	Name            string
	Instructions    string
	SessionID       string
	Model           model.ChatModel
	Toolkit         *tool.Toolkit
	StateSaver      agent.StateSaver
	Context         agent.ContextConfig
	Permissions     *permission.Context
	ToolConcurrency int
	ExclusiveTools  []string
	Hooks           []loop.Hook
}

// NewAgent builds a streaming UnifiedAgent. A nil checkpoint starts a new
// session. To restore a session, load its state with agent.LoadCheckpoint and
// propagate load errors before calling NewAgent. The host scopes the saver
// and supplies a checkpoint, toolkit, permission context and hooks owned by
// this agent. Only permissions supplied by the host apply on restore.
func NewAgent(config Config, checkpoint *agent.AgentState) (*agent.UnifiedAgent, error) {
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.SessionID) == "" || config.Model == nil {
		return nil, errors.New("agent name, session identity and model are required")
	}
	if config.ToolConcurrency <= 0 {
		return nil, errors.New("tool concurrency must be positive")
	}
	if checkpoint == nil {
		checkpoint = &agent.AgentState{SessionID: config.SessionID}
	} else {
		if config.StateSaver == nil {
			return nil, errors.New("restoring an agent requires a state saver")
		}
		if checkpoint.SessionID != config.SessionID {
			return nil, errors.New("checkpoint belongs to a different session")
		}
		if checkpoint.SchemaVersion < 1 || checkpoint.SchemaVersion > agent.StateSchemaVersion {
			return nil, errors.New("unsupported checkpoint schema")
		}
		// AgentScope otherwise merges saved working directories into host permissions.
		restored := *checkpoint
		restored.PermissionCtx = nil
		checkpoint = &restored
	}

	options := []agent.AgentOption{
		agent.WithState(checkpoint),
		agent.WithContextConfig(&config.Context),
		agent.WithToolConcurrencyLimit(config.ToolConcurrency),
		agent.WithExclusiveTools(config.ExclusiveTools...),
		// Legacy middleware disables native provider streaming.
		agent.WithLoopHooks(config.Hooks...),
	}
	if config.Toolkit != nil {
		options = append(options, agent.WithToolkit(config.Toolkit))
	}
	if config.Permissions != nil {
		options = append(options, agent.WithPermissionContext(config.Permissions))
	}
	if config.StateSaver != nil {
		options = append(options,
			agent.WithStateSaver(config.StateSaver),
			agent.WithReplyRecovery(),
		)
	}
	return agent.NewUnifiedAgent(config.Name, config.Instructions, config.Model, options...), nil
}

package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"

	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func handleAskUser(ctx context.Context, runtime *agent.UnifiedAgent, saver *storage.AgentStateSaver,
	request event.RequireExternalExecutionEvent, secretForms []string,
) (bool, error) {
	call := request.ToolCalls[0]
	if call.ID == "" || request.ReplyID == "" || len(call.Input) > 8192 {
		return false, storage.ErrAgentCheckpoint
	}
	// AgentScope validates the native questions before emitting the external call.
	var input tool.AskUserParams
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
		return false, storage.ErrAgentCheckpoint
	}
	safe := func(text string) bool {
		return !strings.ContainsRune(text, 0) && redactModelSecret(text, secretForms) == text
	}
	for _, question := range input.Questions {
		if !safe(question.Question) || !safe(question.Header) || !safe(question.Context) {
			return false, storage.ErrAgentCheckpoint
		}
		for _, option := range question.Options {
			if !safe(option.Label) || !safe(option.Description) || !safe(option.Preview) {
				return false, storage.ErrAgentCheckpoint
			}
		}
	}

	if err := runtime.SaveCheckpoint(ctx); err != nil {
		return false, err
	}
	result, err := saver.ParkAskUser(ctx, request.ReplyID, call, input.Questions)
	if err != nil {
		return false, err
	}
	if result == nil {
		return true, nil
	}
	answer := event.NewExternalExecutionResultEvent(
		request.ReplyID, []message.ToolResultBlock{*result},
	)
	return false, runtime.SubmitExternalResultContext(ctx, &answer)
}

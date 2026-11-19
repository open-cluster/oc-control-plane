package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	executionLease       = 30 * time.Second
	executionHeartbeat   = 3 * time.Second
	executionPoll        = 500 * time.Millisecond
	executionBudget      = 30 * time.Minute
	executionResultLimit = 15 * time.Second
	completedReplyMarker = "opencluster.completed_reply"
)

// ExecutionProfile supplies current authorized agent configuration for a claim.
// Investigation result validation remains with the caller that owns the profile.
type ExecutionProfile struct {
	ai.Config
	// Conclusion validates and projects a saved reply without calling the model.
	Conclusion func(context.Context, string) (investigation.Conclusion, error)
}

type PrepareExecution func(context.Context, uuid.UUID, storage.AgentExecution) (ExecutionProfile, error)

// RunExecutions owns worker lifetime. A wakeup after admission accelerates polling.
func RunExecutions(ctx context.Context, db *storage.Database, workers int, wake <-chan struct{}, prepare PrepareExecution, logger *slog.Logger) {
	var running sync.WaitGroup
	for range max(1, workers) {
		running.Go(func() {
			runExecutionWorker(ctx, db, wake, prepare, logger)
		})
	}
	running.Wait()
}

func runExecutionWorker(ctx context.Context, db *storage.Database, wake <-chan struct{}, prepare PrepareExecution, logger *slog.Logger) {
	for ctx.Err() == nil {
		org, execution, err := db.ClaimNextAgentExecution(ctx, executionLease)
		if err == nil {
			err = runClaimedExecution(ctx, db, org, execution, prepare, logger)
			if err == nil || errors.Is(err, storage.ErrAgentLeaseLost) {
				continue
			}
		}
		if logger != nil && ctx.Err() == nil && !errors.Is(err, storage.ErrAgentExecutionUnavailable) {
			logger.WarnContext(ctx, "durable agent work was interrupted",
				slog.String("org_id", org.String()), slog.String("execution_id", execution.ID.String()))
		}
		timer := time.NewTimer(executionPoll)
		select {
		case <-ctx.Done():
		case <-timer.C:
		case _, open := <-wake:
			if !open {
				wake = nil
			}
		}
		timer.Stop()
	}
}

func runClaimedExecution(processCtx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution, prepare PrepareExecution, logger *slog.Logger) error {
	ctx, revoke := context.WithCancelCause(processCtx)
	monitored := make(chan struct{})
	go func() {
		defer close(monitored)
		renewExecution(ctx, db, org, execution, revoke)
	}()
	defer func() {
		revoke(nil)
		<-monitored
	}()

	saver := db.AgentStateSaver(org, execution)
	checkpoint, writer, err := saver.Restore(ctx)
	if errors.Is(err, storage.ErrAgentCheckpoint) {
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	if err != nil {
		return err
	}

	if writer == execution.ID {
		recovery := checkpoint.ReplyRecovery
		if recovery == nil || recovery.Version != 1 || recovery.ReplyID == "" || recovery.ReplyID != checkpoint.ReplyID {
			return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
		}
		if !recovery.Active {
			if execution.InvestigationID != uuid.Nil && logger != nil {
				logger.InfoContext(ctx, "agent token usage incomplete after recovery", slog.String("execution_id", execution.ID.String()))
			}
			return finishAgentReply(ctx, db, org, execution, checkpoint, prepare, nil, investigation.Usage{})
		}
	}

	runCtx, stop := context.WithDeadline(ctx, execution.StartedAt.Add(executionBudget))
	defer stop()
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}

	profile, err := prepare(runCtx, org, execution)
	if err != nil || execution.InvestigationID != uuid.Nil && profile.Conclusion == nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	lifecycle := &executionReply{AgentStateSaver: saver, done: make(chan struct{})}
	profile.SessionID = execution.SessionID()
	profile.StateSaver = lifecycle
	profile.Hooks = append(slices.Clone(profile.Hooks), lifecycle)

	runtime, err := ai.NewAgent(profile.Config, checkpoint)
	if err != nil {
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	var stream <-chan event.Event
	recovering := writer == execution.ID
	if recovering {
		if err := db.RenewAgentExecution(runCtx, org, execution, executionLease); err != nil {
			return err
		}
		stream, err = runtime.ResumeReplyStream(runCtx)
	} else {
		input, inputErr := db.AgentExecutionInput(runCtx, org, execution)
		if inputErr != nil {
			return inputErr
		}
		stream, err = runtime.ReplyStream(runCtx, input)
	}
	if err != nil {
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	outcome, runErr := drainExecution(runCtx, stream)
	stop()
	// The outward stream can close while native workers still own Tool calls.
	select {
	case <-processCtx.Done():
		return processCtx.Err()
	case <-lifecycle.done:
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if outcome.unsupportedInput {
		return failAgentExecution(ctx, db, org, execution, outcome.usage)
	}
	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) {
		return runErr
	}
	checkpoint, writer, err = saver.Restore(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrAgentCheckpoint) {
			return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
		}
		return err
	}
	if writer != execution.ID || checkpoint.ReplyRecovery == nil || checkpoint.ReplyRecovery.Active {
		if runErr != nil {
			return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
		}
		return errors.New("agent stream closed before its terminal checkpoint")
	}
	if runErr == nil && !outcome.completed {
		return failAgentExecution(ctx, db, org, execution, outcome.usage)
	}
	if recovering && execution.InvestigationID != uuid.Nil && logger != nil {
		logger.InfoContext(ctx, "agent token usage incomplete after recovery", slog.String("execution_id", execution.ID.String()))
	}
	return finishAgentReply(ctx, db, org, execution, checkpoint, prepare, profile.Conclusion, outcome.usage)
}

func renewExecution(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution, revoke context.CancelCauseFunc) {
	renew := time.NewTicker(executionHeartbeat)
	defer renew.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-renew.C:
			heartbeat, cancel := context.WithTimeout(ctx, executionHeartbeat)
			err := db.RenewAgentExecution(heartbeat, org, execution, executionLease)
			cancel()
			if err != nil {
				revoke(err)
				return
			}
		}
	}
}

type executionOutcome struct {
	completed        bool
	unsupportedInput bool
	usage            investigation.Usage
}

func drainExecution(ctx context.Context, stream <-chan event.Event) (executionOutcome, error) {
	var outcome executionOutcome
	for {
		select {
		case <-ctx.Done():
			return outcome, context.Cause(ctx)
		case item, open := <-stream:
			if !open {
				return outcome, nil
			}
			switch item := item.(type) {
			case event.RequireUserConfirmEvent, event.RequireExternalExecutionEvent:
				outcome.unsupportedInput = true
				return outcome, nil
			case event.ReplyEndEvent:
				outcome.completed = item.FinishedReason == types.ReplyCompleted && item.Error == nil
			case event.ModelCallEndEvent:
				outcome.usage.InputTokens += int64(item.InputTokens) + int64(item.CacheCreationTokens) + int64(item.CacheReadTokens)
				outcome.usage.OutputTokens += int64(item.OutputTokens)
			}
		}
	}
}

func finishAgentReply(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	checkpoint *agent.AgentState, prepare PrepareExecution,
	validate func(context.Context, string) (investigation.Conclusion, error), usage investigation.Usage,
) error {
	if checkpoint.MiddlewareState[completedReplyMarker] != checkpoint.ReplyID {
		return failAgentExecution(ctx, db, org, execution, usage)
	}
	reply := finalReplyContent(checkpoint)
	if strings.TrimSpace(reply) == "" ||
		execution.ConversationID != uuid.Nil && utf8.RuneCountInString(reply) > conversation.MaxMessageTextLength {
		return failAgentExecution(ctx, db, org, execution, usage)
	}
	var conclusion *investigation.Conclusion
	if execution.InvestigationID != uuid.Nil {
		resultCtx, cancel := context.WithTimeout(ctx, executionResultLimit)
		defer cancel()
		if validate == nil {
			profile, err := prepare(resultCtx, org, execution)
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err != nil || profile.Conclusion == nil || resultCtx.Err() != nil {
				return failAgentExecution(ctx, db, org, execution, usage)
			}
			validate = profile.Conclusion
		}
		validated, err := validate(resultCtx, reply)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil || resultCtx.Err() != nil {
			return failAgentExecution(ctx, db, org, execution, usage)
		}
		conclusion = &validated
	}
	return db.FinishAgentExecution(ctx, org, execution, storage.AgentExecutionCompleted, reply, conclusion, usage)
}

func failAgentExecution(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	usage investigation.Usage,
) error {
	return db.FinishAgentExecution(ctx, org, execution, storage.AgentExecutionFailed, "", nil, usage)
}

func finalReplyContent(state *agent.AgentState) string {
	for _, msg := range slices.Backward(state.Context) {
		if msg == nil || msg.Role != message.RoleAssistant || msg.ID != state.ReplyID {
			continue
		}
		// Earlier Tool rounds share this Message.
		start := 0
		for i, block := range msg.Content {
			switch block.(type) {
			case message.ToolCallBlock, message.ToolResultBlock:
				start = i + 1
			}
		}
		var text strings.Builder
		for _, block := range msg.Content[start:] {
			if part, ok := block.(message.TextBlock); ok {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	return ""
}

type executionReply struct {
	loop.HookRunner
	*storage.AgentStateSaver
	completed atomic.Bool
	done      chan struct{}
}

func (r *executionReply) OnStateTransition(from, to protocol.LoopState, _ int) {
	if from == protocol.StateInspect && to == protocol.StateExit {
		r.completed.Store(true)
	}
}

func (r *executionReply) OnLoopEnd(error) { close(r.done) }

func (r *executionReply) SaveState(ctx context.Context, session string, state *agent.AgentState) error {
	// The native inactive flag also covers errors and forced budget summaries.
	if state.ReplyRecovery != nil && !state.ReplyRecovery.Active && r.completed.Load() {
		if state.MiddlewareState == nil {
			state.MiddlewareState = make(map[string]any)
		}
		state.MiddlewareState[completedReplyMarker] = state.ReplyID
	}
	return r.AgentStateSaver.SaveState(ctx, session, state)
}

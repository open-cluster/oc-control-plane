package ai

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	executionLease       = 30 * time.Second
	executionHeartbeat   = 3 * time.Second
	executionPoll        = 500 * time.Millisecond
	executionBudget      = 30 * time.Minute
	completedReplyMarker = "opencluster.completed_reply"
)

// ExecutionProfile supplies current authorized agent configuration for a claim.
// Investigation result validation remains with the caller that owns the profile.
type ExecutionProfile struct {
	Config
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
			err = runClaimedExecution(ctx, db, org, execution, prepare)
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

func runClaimedExecution(processCtx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution, prepare PrepareExecution) error {
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
		return failAgentExecution(ctx, db, org, execution)
	}
	if err != nil {
		return err
	}

	if writer == execution.ID {
		recovery := checkpoint.ReplyRecovery
		if recovery == nil || recovery.Version != 1 || recovery.ReplyID == "" || recovery.ReplyID != checkpoint.ReplyID {
			return failAgentExecution(ctx, db, org, execution)
		}
		if !recovery.Active {
			return reconcileAgentReply(ctx, db, org, execution, checkpoint, prepare)
		}
	}

	runCtx, stop := context.WithDeadline(ctx, execution.StartedAt.Add(executionBudget))
	defer stop()
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failAgentExecution(ctx, db, org, execution)
	}

	profile, err := prepare(runCtx, org, execution)
	if err != nil || execution.InvestigationID != uuid.Nil && profile.Conclusion == nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failAgentExecution(ctx, db, org, execution)
	}
	lifecycle := &executionReply{AgentStateSaver: saver, done: make(chan struct{})}
	profile.SessionID = execution.SessionID()
	profile.StateSaver = lifecycle
	profile.Hooks = append(slices.Clone(profile.Hooks), lifecycle)

	runtime, err := NewAgent(profile.Config, checkpoint)
	if err != nil {
		return failAgentExecution(ctx, db, org, execution)
	}
	var stream <-chan event.Event
	if writer == execution.ID {
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
		return failAgentExecution(ctx, db, org, execution)
	}
	stopStatus, runErr := drainExecution(runCtx, stream)
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
	switch stopStatus {
	case storage.AgentExecutionNeedsInput:
		return db.ParkAgentExecution(ctx, org, execution)
	case storage.AgentExecutionFailed:
		return failAgentExecution(ctx, db, org, execution)
	}
	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) {
		return runErr
	}
	checkpoint, writer, err = saver.Restore(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrAgentCheckpoint) {
			return failAgentExecution(ctx, db, org, execution)
		}
		return err
	}
	if writer != execution.ID || checkpoint.ReplyRecovery == nil || checkpoint.ReplyRecovery.Active {
		if runErr != nil {
			return failAgentExecution(ctx, db, org, execution)
		}
		return errors.New("agent stream closed before its terminal checkpoint")
	}
	return finishAgentReply(ctx, db, org, execution, checkpoint, profile.Conclusion)
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

func drainExecution(ctx context.Context, stream <-chan event.Event) (storage.AgentExecutionStatus, error) {
	for {
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case item, open := <-stream:
			if !open {
				return "", nil
			}
			switch item.(type) {
			case event.RequireUserConfirmEvent:
				return storage.AgentExecutionNeedsInput, nil
			case event.RequireExternalExecutionEvent:
				return storage.AgentExecutionFailed, nil
			}
		}
	}
}

func reconcileAgentReply(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	checkpoint *agent.AgentState, prepare PrepareExecution,
) error {
	ctx, cancel := context.WithTimeout(ctx, executionBudget)
	defer cancel()
	var conclusion func(context.Context, string) (investigation.Conclusion, error)
	if execution.InvestigationID != uuid.Nil && checkpoint.MiddlewareState[completedReplyMarker] == checkpoint.ReplyID {
		profile, err := prepare(ctx, org, execution)
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return failAgentExecution(ctx, db, org, execution)
		}
		conclusion = profile.Conclusion
	}
	return finishAgentReply(ctx, db, org, execution, checkpoint, conclusion)
}

func finishAgentReply(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	checkpoint *agent.AgentState, decode func(context.Context, string) (investigation.Conclusion, error),
) error {
	ctx, cancel := context.WithTimeout(ctx, executionBudget)
	defer cancel()
	if checkpoint.MiddlewareState[completedReplyMarker] != checkpoint.ReplyID {
		return failAgentExecution(ctx, db, org, execution)
	}
	reply, usage := finalReplyContent(checkpoint)
	if strings.TrimSpace(reply) == "" {
		return failAgentExecution(ctx, db, org, execution)
	}
	var conclusion *investigation.Conclusion
	if execution.InvestigationID != uuid.Nil {
		if decode == nil {
			return failAgentExecution(ctx, db, org, execution)
		}
		validated, err := decode(ctx, reply)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil {
			return failAgentExecution(ctx, db, org, execution)
		}
		conclusion = &validated
	}
	return db.FinishAgentExecution(ctx, org, execution, storage.AgentExecutionCompleted, reply, conclusion, usage)
}

func failAgentExecution(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution) error {
	return db.FinishAgentExecution(ctx, org, execution, storage.AgentExecutionFailed, "", nil, investigation.Usage{})
}

func finalReplyContent(state *agent.AgentState) (string, investigation.Usage) {
	for _, msg := range slices.Backward(state.Context) {
		if msg == nil || msg.Role != message.RoleAssistant || msg.ID != state.ReplyID {
			continue
		}
		// Earlier tool rounds share this Message. Only the final text is product output.
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
		var usage investigation.Usage
		if msg.Usage != nil {
			usage.InputTokens = int64(msg.Usage.InputTokens)
			usage.OutputTokens = int64(msg.Usage.OutputTokens)
		}
		return text.String(), usage
	}
	return "", investigation.Usage{}
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

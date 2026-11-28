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
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
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
type ExecutionProfile struct {
	ai.Config
	Input func(context.Context) (string, error)
}

type PrepareExecution func(context.Context, uuid.UUID, storage.AgentExecution) (ExecutionProfile, error)

// RunExecutions owns worker lifetime. A wakeup after admission accelerates polling.
func RunExecutions(ctx context.Context, db *storage.Database, workers int, wake <-chan struct{}, prepare PrepareExecution, logger *slog.Logger) {
	runExecutions(ctx, db, workers, wake, prepare, logger, nil, "")
}

func runExecutions(ctx context.Context, db *storage.Database, workers int, wake <-chan struct{}, prepare PrepareExecution,
	logger *slog.Logger, suggestTitle func(uuid.UUID, uuid.UUID, string), modelSecret string,
) {
	var running sync.WaitGroup
	for range max(1, workers) {
		running.Go(func() {
			runExecutionWorker(ctx, db, wake, prepare, logger, suggestTitle, modelSecret)
		})
	}
	running.Wait()
}

func runExecutionWorker(ctx context.Context, db *storage.Database, wake <-chan struct{}, prepare PrepareExecution,
	logger *slog.Logger, suggestTitle func(uuid.UUID, uuid.UUID, string), modelSecret string,
) {
	for ctx.Err() == nil {
		org, execution, err := db.ClaimNextAgentExecution(ctx, executionLease)
		if err == nil {
			err = runClaimedExecution(ctx, db, org, execution, prepare, suggestTitle, modelSecret)
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

func runClaimedExecution(processCtx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	prepare PrepareExecution, suggestTitle func(uuid.UUID, uuid.UUID, string), modelSecret string,
) error {
	var fallback string
	defer func() {
		if fallback != "" && suggestTitle != nil {
			suggestTitle(org, execution.ConversationID, fallback)
		}
	}()
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
	finishChat := func(checkpoint *agent.AgentState, usage investigation.Usage) error {
		var err error
		fallback, err = finishAgentReply(ctx, db, org, execution, checkpoint, usage, modelSecret)
		return err
	}

	saver := db.AgentStateSaver(org, execution)
	checkpoint, writer, err := saver.Restore(ctx)
	if err == nil {
		err = saver.ValidateAskUserResume(checkpoint)
	}
	if errors.Is(err, storage.ErrAgentCheckpoint) {
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	if err != nil {
		return err
	}

	recovering := writer == execution.ID
	var recoveredUsage investigation.Usage
	if recovering {
		recovery := checkpoint.ReplyRecovery
		if recovery == nil || recovery.Version != 1 || recovery.ReplyID == "" || recovery.ReplyID != checkpoint.ReplyID {
			return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
		}
		if !recovery.Active {
			if !completedReply(checkpoint) {
				return failAgentExecution(ctx, db, org, execution, checkpointUsage(checkpoint))
			}
			if execution.InvestigationID == uuid.Nil {
				return finishChat(checkpoint, checkpointUsage(checkpoint))
			}
		}
		recoveredUsage = checkpointUsage(checkpoint)
	}

	runCtx, stop := context.WithDeadline(ctx, execution.StartedAt.Add(executionBudget+execution.ParkedDuration()))
	defer stop()
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}

	profile, err := prepare(runCtx, org, execution)
	if err != nil {
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
	if recovering && completedReply(checkpoint) {
		return finishInvestigation(runCtx, ctx, db, org, execution, runtime, checkpoint, recoveredUsage)
	}

	var stream <-chan event.Event
	if recovering {
		if err := db.RenewAgentExecution(runCtx, org, execution, executionLease); err != nil {
			return err
		}
		stream, err = runtime.ResumeReplyStream(runCtx)
	} else {
		loadInput := profile.Input
		if loadInput == nil {
			loadInput = func(ctx context.Context) (string, error) {
				return db.AgentExecutionInput(ctx, org, execution)
			}
		}
		input, inputErr := loadInput(runCtx)
		if errors.Is(inputErr, authz.ErrNotAMember) {
			return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
		}
		if inputErr != nil {
			return inputErr
		}
		stream, err = runtime.ReplyStream(runCtx, input)
	}
	if err != nil {
		return failAgentExecution(ctx, db, org, execution, investigation.Usage{})
	}
	var saveDraft func(context.Context, string) error
	if execution.ConversationID != uuid.Nil {
		saveDraft = func(ctx context.Context, text string) error {
			return db.SaveChatDraft(ctx, org, execution, text)
		}
	}
	var askUser func(event.RequireExternalExecutionEvent) (bool, error)
	if profile.Toolkit != nil && profile.Toolkit.Get("AskUser") != nil {
		secretForms := modelSecretForms(modelSecret)
		askUser = func(request event.RequireExternalExecutionEvent) (bool, error) {
			return handleAskUser(runCtx, runtime, saver, request, secretForms)
		}
	}
	outcome, runErr := drainExecution(runCtx, stream, saveDraft, askUser, modelSecret)
	outcome.usage = recoveredUsage.Add(outcome.usage)
	if outcome.unsupportedInput || outcome.parked || runErr != nil {
		stop()
	}
	// The outward stream can close while native workers still own Tool calls.
	select {
	case <-processCtx.Done():
		return processCtx.Err()
	case <-lifecycle.done:
	}
	if outcome.parked {
		for range stream {
		}
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if outcome.unsupportedInput || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, storage.ErrAgentCheckpoint) || errors.Is(runErr, authz.ErrNotAMember) {
		return failAgentExecution(ctx, db, org, execution, outcome.usage)
	}
	if runErr != nil {
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
		return errors.New("agent stream closed before its terminal checkpoint")
	}
	if !outcome.completed || !completedReply(checkpoint) {
		return failAgentExecution(ctx, db, org, execution, outcome.usage)
	}
	if execution.InvestigationID != uuid.Nil {
		return finishInvestigation(runCtx, ctx, db, org, execution, runtime, checkpoint, outcome.usage)
	}
	return finishChat(checkpoint, outcome.usage)
}

func completedReply(state *agent.AgentState) bool {
	return state.ReplyRecovery != nil && !state.ReplyRecovery.Active &&
		state.MiddlewareState[completedReplyMarker] == state.ReplyID
}

func finishInvestigation(runCtx, leaseCtx context.Context, db *storage.Database, org uuid.UUID,
	execution storage.AgentExecution, runtime *agent.UnifiedAgent, checkpoint *agent.AgentState, usage investigation.Usage,
) error {
	assigned, err := db.AuthorizeInvestigationReport(runCtx, org, execution)
	if err != nil {
		if errors.Is(err, authz.ErrNotAMember) || errors.Is(err, conversation.ErrUnknown) ||
			errors.Is(err, storage.ErrAgentToolRunUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			return failAgentExecution(leaseCtx, db, org, execution, usage)
		}
		return err
	}
	instructions := ai.ConclusionInstructions
	schema := investigation.ConclusionSchema()
	var replyIndex, textStart int
	if assigned {
		replyIndex, textStart = terminalReply(checkpoint)
		if replyIndex < 0 {
			return failAgentExecution(leaseCtx, db, org, execution, usage)
		}
		instructions += "\nFor a factual question without a causal assessment, put the direct answer in the summary and omit status."
		schema = investigation.AssignedMessageConclusionSchema()
	}
	// Observe only affects the live agent. The detached checkpoint is the source for promotion.
	instruction := message.UserMsg("operator", instructions)
	if err := runtime.Observe(runCtx, []*message.Msg{instruction}); err != nil {
		return failAgentExecution(leaseCtx, db, org, execution, usage)
	}
	report, err := runtime.GenerateStructuredResponseWithUsage(runCtx, schema)
	if leaseCtx.Err() != nil {
		return context.Cause(leaseCtx)
	}
	if report != nil && report.Usage != nil {
		usage.InputTokens += int64(report.Usage.EffectiveInputTokens())
		usage.OutputTokens += int64(report.Usage.OutputTokens)
	}
	if err != nil {
		return failAgentExecution(leaseCtx, db, org, execution, usage)
	}
	conclusion, err := investigation.ParseConclusion(report.Output)
	if err != nil {
		return failAgentExecution(leaseCtx, db, org, execution, usage)
	}

	// Publish only the accepted visible reply with Conversation context.
	if assigned {
		checkpoint.Context[replyIndex] = replaceFinalReply(checkpoint.Context[replyIndex], textStart, conclusion.Summary)
	}
	err = db.FinishAgentExecution(leaseCtx, org, execution, storage.AgentExecutionCompleted,
		conclusion.Summary, &conclusion, usage, checkpoint)
	if errors.Is(err, storage.ErrAgentEvidenceUnavailable) || errors.Is(err, investigation.ErrInvalidConclusion) ||
		errors.Is(err, authz.ErrNotAMember) || errors.Is(err, storage.ErrAgentToolRunUnavailable) {
		return failAgentExecution(leaseCtx, db, org, execution, usage)
	}
	return err
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
	parked           bool
	usage            investigation.Usage
}

func drainExecution(ctx context.Context, stream <-chan event.Event, saveDraft func(context.Context, string) error,
	askUser func(event.RequireExternalExecutionEvent) (bool, error), modelSecret string,
) (executionOutcome, error) {
	var outcome executionOutcome
	draft := executionDraft{persist: saveDraft, secretForms: modelSecretForms(modelSecret), savedText: new("")}
	defer draft.stopTimer()
	for {
		select {
		case <-ctx.Done():
			return outcome, context.Cause(ctx)
		case <-draft.flushReady():
			draft.timer = nil
			draft.flush(ctx)
		case item, open := <-stream:
			if !open {
				if outcome.completed {
					draft.flush(ctx)
				}
				return outcome, nil
			}
			switch item := item.(type) {
			case event.RequireUserConfirmEvent:
				outcome.unsupportedInput = true
				return outcome, nil
			case event.RequireExternalExecutionEvent:
				if askUser == nil || len(item.ToolCalls) != 1 || item.ToolCalls[0].Name != "AskUser" {
					outcome.unsupportedInput = true
					return outcome, nil
				}
				parked, err := askUser(item)
				if parked || err != nil {
					outcome.parked = parked
					return outcome, err
				}
			case event.ReplyEndEvent:
				outcome.completed = item.FinishedReason == types.ReplyCompleted && item.Error == nil
			case event.ModelCallStartEvent:
				draft.startModelCall(ctx)
			case event.TextBlockDeltaEvent:
				draft.appendText(ctx, item.Delta)
			case event.ModelCallEndEvent:
				outcome.usage.InputTokens += int64(item.InputTokens) + int64(item.CacheCreationTokens) + int64(item.CacheReadTokens)
				outcome.usage.OutputTokens += int64(item.OutputTokens)
			}
		}
	}
}

func finishAgentReply(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	checkpoint *agent.AgentState, usage investigation.Usage, modelSecret string,
) (string, error) {
	index, start := terminalReply(checkpoint)
	if index < 0 {
		return "", failAgentExecution(ctx, db, org, execution, usage)
	}
	nativeMessage := checkpoint.Context[index]
	var text strings.Builder
	for _, block := range nativeMessage.Content[start:] {
		if part, ok := block.(message.TextBlock); ok {
			text.WriteString(part.Text)
		}
	}
	native := text.String()
	reply := redactModelSecret(strings.ReplaceAll(native, "\x00", ""), modelSecretForms(modelSecret))
	if strings.TrimSpace(reply) == "" || utf8.RuneCountInString(reply) > conversation.MaxMessageTextLength {
		return "", failAgentExecution(ctx, db, org, execution, usage)
	}
	var promoted []*agent.AgentState
	if reply != native {
		checkpoint.Context[index] = replaceFinalReply(nativeMessage, start, reply)
		promoted = append(promoted, checkpoint)
	}
	fallback, err := db.FinishAgentChat(ctx, org, execution, reply, usage, promoted...)
	if errors.Is(err, authz.ErrNotAMember) || errors.Is(err, storage.ErrAgentToolRunUnavailable) {
		return "", failAgentExecution(ctx, db, org, execution, usage)
	}
	return fallback, err
}

func failAgentExecution(ctx context.Context, db *storage.Database, org uuid.UUID, execution storage.AgentExecution,
	usage investigation.Usage,
) error {
	return db.FinishAgentExecution(ctx, org, execution, storage.AgentExecutionFailed, "", nil, usage)
}

func terminalReply(state *agent.AgentState) (int, int) {
	for index, msg := range slices.Backward(state.Context) {
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
		return index, start
	}
	return -1, 0
}

func replaceFinalReply(native *message.Msg, start int, answer string) *message.Msg {
	// Keep native block identities and opaque provider continuation blocks intact.
	reply := *native
	reply.Content = slices.Clone(native.Content)
	lastText := -1
	for i := start; i < len(reply.Content); i++ {
		if _, ok := reply.Content[i].(message.TextBlock); ok {
			lastText = i
		}
	}
	if lastText < 0 {
		reply.Content = append(reply.Content, message.TextBlock{Type: "text", Text: answer})
		return &reply
	}

	remaining := []rune(answer)
	for i := start; i <= lastText; i++ {
		block, ok := reply.Content[i].(message.TextBlock)
		if !ok {
			continue
		}
		count := min(utf8.RuneCountInString(block.Text), len(remaining))
		if i == lastText {
			count = len(remaining)
		}
		block.Text = string(remaining[:count])
		remaining = remaining[count:]
		reply.Content[i] = block
	}
	return &reply
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

func checkpointUsage(state *agent.AgentState) investigation.Usage {
	if state != nil {
		for _, msg := range slices.Backward(state.Context) {
			if msg != nil && msg.ID == state.ReplyID && msg.Role == message.RoleAssistant && msg.Usage != nil {
				return investigation.Usage{
					InputTokens:  int64(msg.Usage.InputTokens),
					OutputTokens: int64(msg.Usage.OutputTokens),
				}
			}
		}
	}
	return investigation.Usage{}
}

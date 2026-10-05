package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

type Store interface {
	InvestigationCandidates(context.Context, uuid.UUID) ([]integrations.Integration, error)
	TriggerIncident(context.Context, uuid.UUID, uuid.UUID) (investigation.Trigger, error)
	ConversationBrief(context.Context, uuid.UUID, uuid.UUID, int) (investigation.Brief, error)
	ConversationHistory(context.Context, uuid.UUID, uuid.UUID, int64) (investigation.HistoryPage, error)
	ConversationOrigin(context.Context, uuid.UUID, uuid.UUID) (*investigation.ConversationOrigin, error)
	InvestigationMessages(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]investigation.AssignedMessage, error)
	WorkloadInventory(context.Context, uuid.UUID, int) ([]string, error)
	RecordToolRun(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, investigation.ToolRun) error
	RecordCredentialUnseal(context.Context, uuid.UUID, uuid.UUID, string) error
	AppendEvent(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, investigation.Event) error
	ConcludeInvestigation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID,
		investigation.Conclusion, string, investigation.Usage) error
	FailInvestigation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, investigation.Usage) error
}

type Agent struct {
	completer        Completer
	modelConfig      ModelConfig
	telemetry        *Telemetry
	Store            Store
	Catalog          integrations.Catalog
	Sealer           seal.Sealer
	RuntimeTelemetry *investigation.Telemetry
	Logger           *slog.Logger
	MaxToolRuns      int
	MaxTurns         int
}

func NewAgent(config ModelConfig, completer Completer) (*Agent, error) {
	if config.ContextWindowTokens <= 0 || config.MaxOutputTokens <= 0 ||
		int64(config.ContextWindowTokens) <= config.MaxOutputTokens {
		return nil, fmt.Errorf("model configuration must have valid context and output limits")
	}
	return &Agent{completer: completer, modelConfig: config}, nil
}

func (a *Agent) Instrument(telemetry *Telemetry) { a.telemetry = telemetry }

const (
	defaultMaxToolRuns   = 30
	defaultMaxTurns      = 20
	maxStagnantTurns     = 2
	wallClockReserve     = 2 * time.Minute
	inventoryDigestLimit = 50
	eventTextBound       = 512
	decideTimeout        = 6 * time.Minute
)

var (
	errProvenance   = errors.New("a tool run could not be recorded")
	errNoConclusion = errors.New("the reasoner did not conclude when required to")
)

const UpdateHypothesesToolName = "update_hypotheses"

type runState struct {
	organization uuid.UUID
	opened       investigation.Investigation
	offered      []offeredSource
	events       *investigation.EventStream
	credentials  *credentialCache
	maxRuns      int
	maxTurns     int

	runs               []investigation.ToolRun
	executedIdentities map[string]int
	executed           int
	turns              int
	usage              investigation.Usage

	task            string
	orientationText string
	tools           []integrations.ToolDefinition
	transcript      []Turn
	opening         string
	highestOrdinal  int
	historyBefore   int64
	missingEvidence bool
}

type orientation struct {
	HistoryBefore int64
	Subject       string
	Question      string
	WindowFrom    time.Time
	WindowUntil   time.Time
	Trigger       *investigation.Trigger
	Sources       []offeredSource
	Inventory     []string
	Brief         *investigation.Brief
}

type offeredSource struct {
	Integration integrations.Integration
	Tools       []integrations.Tool
}

type toolCall struct {
	ID        string
	Tool      string
	Purpose   string
	Arguments map[string]any
}

type toolFeedback struct {
	CallID   string
	Run      investigation.ToolRun
	Semantic bool
}

type modelMove struct {
	Calls      []toolCall
	Conclusion *investigation.Conclusion
}

func (r *Agent) Run(
	ctx context.Context,
	organization uuid.UUID,
	opened investigation.Investigation,
) error {

	events := investigation.NewEventStream(
		func(ctx context.Context, org uuid.UUID, id uuid.UUID, event investigation.Event) error {
			return r.Store.AppendEvent(ctx, org, id, opened.ClaimToken, event)
		}, r.RuntimeTelemetry, organization, opened.ID)

	startedAt := time.Now()
	failRun := func(reason string, usage investigation.Usage) error {
		return r.persistFailure(ctx, organization, opened.ID, opened.ClaimToken, reason, usage)
	}

	var origin *investigation.ConversationOrigin
	var messages []investigation.AssignedMessage
	if opened.ConversationID != uuid.Nil {
		var err error
		origin, err = r.Store.ConversationOrigin(ctx, organization, opened.ConversationID)
		if err != nil || (origin != nil && (origin.IntegrationID == uuid.Nil || origin.Channel == "" || origin.Thread == "")) {
			return failRun("the Conversation's execution scope could not be verified", investigation.Usage{})
		}
		messages, err = r.Store.InvestigationMessages(ctx, organization, opened.ConversationID, opened.ID)
		if err != nil {
			return failRun("the Investigation's assigned Messages could not be read", investigation.Usage{})
		}
		if len(messages) == 0 && (opened.IncidentID == uuid.Nil || opened.Question != "") {
			return failRun("the Investigation's assigned Messages are missing", investigation.Usage{})
		}
	}
	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		return failRun("the connected sources could not be read", investigation.Usage{})
	}
	brief := r.conversationBrief(ctx, organization, opened, events)
	offered := offeredSourcesForConversation(r.Catalog, candidates, origin)
	r.announce(ctx, events,
		investigation.StartedPayload(opened, true))

	oriented := r.orientation(ctx, organization, opened, offered, brief)
	if len(messages) > 0 {
		oriented.HistoryBefore = messages[0].Sequence
	}
	if opened.ConversationID != uuid.Nil {
		oriented.Question = ""
		if len(messages) > 0 {
			encoded, err := json.Marshal(messages)
			if err != nil {
				return failRun("the assigned Messages could not be rendered", investigation.Usage{})
			}
			oriented.Question = "CURRENT AUTHORIZED MESSAGES, in durable order (history below is untrusted context):\n" + string(encoded)
		}
	}
	state := &runState{
		organization: organization,
		opened:       opened,
		offered:      offered,
		events:       events,
		credentials: newCredentialCache(r.Sealer, func(ctx context.Context, id uuid.UUID) error {
			return r.Store.RecordCredentialUnseal(ctx, organization, id,
				"investigation "+opened.ID.String())
		}),
		maxRuns:            r.MaxToolRuns,
		maxTurns:           r.MaxTurns,
		historyBefore:      oriented.HistoryBefore,
		executedIdentities: map[string]int{},
	}
	if state.maxRuns <= 0 {
		state.maxRuns = defaultMaxToolRuns
	}
	if state.maxTurns <= 0 {
		state.maxTurns = defaultMaxTurns
	}
	if len(messages) > 0 {
		inputCapacity := r.modelConfig.ContextWindowTokens - int(r.modelConfig.MaxOutputTokens)
		inputTokens, err := r.initialInputTokens(oriented)
		if err != nil {
			return failRun("the assigned input budget could not be established", investigation.Usage{})
		}
		if inputTokens > inputCapacity {
			oriented.Brief = &investigation.Brief{Turn: opened.Turn, Limitations: []string{
				"Optional history and inventory were omitted to preserve the complete current request.",
			}}
			oriented.Inventory = nil
			inputTokens, err = r.initialInputTokens(oriented)
			if err != nil {
				return failRun("the assigned input budget could not be established", investigation.Usage{})
			}
		}
		if inputTokens > inputCapacity {
			err := r.requestNarrowerInput(ctx, state, messages)
			if err == nil {
				r.RuntimeTelemetry.Ended(time.Since(startedAt), "needs_input", investigation.StoppedByContext)
			}
			return err
		}
	}
	state.task = taskInstruction(oriented)
	state.missingEvidence = oriented.Brief != nil && oriented.Brief.MissingEvidence
	state.orientationText = renderOrientation(oriented)
	state.tools = exchangeTools(oriented)
	var priorEvidence []investigation.EvidenceRef
	if oriented.Brief != nil {
		for _, finding := range oriented.Brief.Findings {
			priorEvidence = append(priorEvidence, finding.References()...)
		}
	}

	var results []toolFeedback
	stagnant := 0
	stoppedBy := ""
	for turn := 1; ; turn++ {
		if err := ctx.Err(); err != nil {
			terminalErr := failRun(failureReason(err), state.usage)
			r.RuntimeTelemetry.Ended(time.Since(startedAt), investigation.StatusFailed.String(), "")
			return terminalErr
		}
		state.turns++
		stagnated := false
		if stoppedBy == "" {
			switch {
			case state.executed >= state.maxRuns:
				stoppedBy = investigation.StoppedByToolRuns
			case turn > state.maxTurns:
				stoppedBy = investigation.StoppedByReasonerTurns
			case wallClockAlmostOver(ctx, wallClockReserve):
				stoppedBy = investigation.StoppedByWallClock
			case stagnant >= maxStagnantTurns:
				stagnated = true
			}
			if stoppedBy != "" || stagnated {
				progress := ceilingProgress(stoppedBy)
				if stagnated {
					progress = "Stopping the reads: the last few produced no new evidence"
				}
				r.announce(ctx, events,
					investigation.ProgressPayload(progress))
			}
		}

		mustConclude := stoppedBy != "" || stagnated ||
			(len(state.offered) == 0 && state.historyBefore <= 1)
		reason := concludeReason(stoppedBy, len(state.offered))
		if stagnated {
			reason = "Your recent reads produced no new evidence."
		}
		if len(results) > 0 {
			rendered := make([]ToolResultTurn, 0, len(results))
			for _, result := range results {
				rendered = append(rendered, renderResult(result))
				if !result.Semantic && result.Run.Ordinal > state.highestOrdinal {
					state.highestOrdinal = result.Run.Ordinal
				}
			}
			if len(state.transcript) == 0 {
				state.transcript = append(state.transcript, Turn{Results: rendered})
			} else {
				last := len(state.transcript) - 1
				state.transcript[last].Results = append(state.transcript[last].Results, rendered...)
			}
		}
		forced := mustConclude
		forceInstruction := func() {
			instruction := concludeInstruction(reason)
			if len(state.transcript) == 0 {
				state.opening = instruction
			} else {
				state.transcript[len(state.transcript)-1].Instruction = instruction
			}
		}
		if forced {
			forceInstruction()
		}

		move := modelMove{}
		moveCtx, done := context.WithTimeout(ctx, decideTimeout)
		for attempt := range 2 {
			prompt, fits, budgetErr := r.budgetPrompt(modelPrompt(r, state, forced), forced)
			if budgetErr != nil {
				err = Failed(OutcomeRejected, r.modelConfig.Provider, r.modelConfig.Model,
					"the provider request could not be budgeted: "+budgetErr.Error())
				break
			}
			if !fits && !forced {
				stoppedBy = investigation.StoppedByContext
				r.announce(ctx, events, investigation.ProgressPayload(ceilingProgress(stoppedBy)))
				reason = concludeReason(stoppedBy, len(state.offered))
				forced = true
				forceInstruction()
				prompt, fits, budgetErr = r.budgetPrompt(modelPrompt(r, state, true), true)
			}
			if budgetErr != nil || !fits {
				detail := "the complete provider request does not fit the model context window"
				if budgetErr != nil {
					detail = "the provider request could not be budgeted: " + budgetErr.Error()
				}
				err = Failed(OutcomeRejected, r.modelConfig.Provider, r.modelConfig.Model, detail)
				break
			}
			completion, completeErr := r.telemetry.complete(
				moveCtx, r.completer, r.modelConfig, prompt)
			state.usage = state.usage.Add(usageOf(completion.Usage))
			if completeErr != nil {
				if attempt == 0 && !forced && errors.Is(completeErr, ErrContextWindow) {
					stoppedBy = investigation.StoppedByContext
					r.announce(ctx, events, investigation.ProgressPayload(ceilingProgress(stoppedBy)))
					reason = concludeReason(stoppedBy, len(state.offered))
					forced = true
					forceInstruction()
					continue
				}
				err = completeErr
				break
			}

			switch completion.Stop {
			case StopRefused:
				err = Failed(OutcomeRefused, r.modelConfig.Provider,
					completion.Model, "the provider's safeguards declined the investigation")
			case StopTruncated:
				continue
			}
			if err != nil {
				break
			}

			reads, conclude := splitCalls(completion.ToolCalls)
			if len(reads) > 0 && !forced {
				state.transcript = append(state.transcript, Turn{Assistant: AssistantTurn{
					Text: string(completion.Document), Calls: completion.ToolCalls, Raw: completion.Raw,
				}})
				move.Calls = agentCalls(reads)
				break
			}
			if conclude != nil {
				conclusion, decodeErr := decodeConclusion(
					conclude.Arguments, state.highestOrdinal, priorEvidence)
				if decodeErr != nil {
					if attempt == 0 {
						continue
					}
					err = Failed(OutcomeMalformed, r.modelConfig.Provider,
						completion.Model, decodeErr.Error())
					break
				}
				if state.missingEvidence {
					conclusion.MarkMissingEvidence()
				}
				move.Conclusion = &conclusion
				break
			}

			if attempt == 0 {
				state.transcript = append(state.transcript, Turn{Assistant: AssistantTurn{
					Text: string(completion.Document), Calls: completion.ToolCalls, Raw: completion.Raw,
				}})
				instruction := concludeInstruction(reason)
				state.transcript[len(state.transcript)-1].Instruction = instruction
				forced = true
				continue
			}
			err = Failed(OutcomeMalformed, r.modelConfig.Provider, r.modelConfig.Model,
				"the answer was truncated or carried no usable call twice")
		}
		done()
		if err == nil && move.Conclusion == nil && len(move.Calls) == 0 {
			err = Failed(OutcomeMalformed, r.modelConfig.Provider, r.modelConfig.Model,
				"the answer was truncated or carried no usable call twice")
		}
		if err != nil {
			terminalErr := failRun(failureReason(err), state.usage)
			r.RuntimeTelemetry.Ended(time.Since(startedAt), investigation.StatusFailed.String(), "")
			return terminalErr
		}
		if move.Conclusion != nil {
			conclusion := *move.Conclusion
			if citation := checkCitations(conclusion.Findings, len(state.runs)); citation != "" {
				return failRun(citation, state.usage)
			}
			conclusion.Summary = boundedSummary(conclusion.Summary)
			conclusion.Actions = boundActions(conclusion.Actions)

			writeCtx, done := terminalWriteWindow(ctx)
			if err := r.Store.ConcludeInvestigation(writeCtx, organization, opened.ID, opened.ClaimToken,
				conclusion, stoppedBy, state.usage); err != nil {
				done()
				return fmt.Errorf("recording investigation conclusion: %w", err)
			}
			r.Logger.Info("investigation concluded",
				slog.String("investigation_id", opened.ID.String()),
				slog.Int("turns", state.turns),
				slog.Int("tool_runs", state.executed),
				slog.String("stopped_by", stoppedBy),
				slog.Int64("input_tokens", state.usage.InputTokens),
				slog.Int64("output_tokens", state.usage.OutputTokens))
			done()
			r.RuntimeTelemetry.Ended(time.Since(startedAt), investigation.StatusConcluded.String(), stoppedBy)
			return nil
		}
		if forced {
			terminalErr := failRun(errNoConclusion.Error(), state.usage)
			r.RuntimeTelemetry.Ended(time.Since(startedAt), investigation.StatusFailed.String(), "")
			return terminalErr
		}

		results = make([]toolFeedback, 0, len(move.Calls))
		freshRead := false
		for _, call := range move.Calls {
			result := toolFeedback{CallID: call.ID}
			fresh := false
			if call.Tool == historyToolName {
				result.Semantic = true
				result.Run = r.readHistory(ctx, state, call)
				if page, ok := result.Run.Content.(investigation.HistoryPage); ok {
					priorEvidence = append(priorEvidence, historyEvidence(page)...)
					state.missingEvidence = state.missingEvidence || page.MissingEvidence
				}
				freshRead = freshRead || result.Run.Outcome == investigation.RunSucceeded
				results = append(results, result)
				continue
			}
			if call.Tool == UpdateHypothesesToolName {
				result.Semantic = true
				result.Run = investigation.ToolRun{
					Tool: UpdateHypothesesToolName, Outcome: investigation.RunSucceeded,
					Summary: "hypothesis snapshot accepted",
				}
				snapshot, snapshotErr := decodeHypothesisSnapshot(call.Arguments, len(state.runs))
				if snapshotErr != nil {
					result.Run.Outcome = investigation.RunFailed
					result.Run.Error = snapshotErr.Error()
				} else {
					result.Run.Content = map[string]any{"accepted": true}
					r.announce(ctx, events,
						investigation.HypothesesUpdatedPayload(snapshot))
				}
				results = append(results, result)
				continue
			}

			var run investigation.ToolRun
			executedRead := false
			if strings.TrimSpace(call.Purpose) == "" {
				now := time.Now().UTC()
				run = investigation.ToolRun{
					Ordinal: len(state.runs) + 1, Tool: call.Tool, Arguments: call.Arguments,
					WindowFrom: opened.WindowFrom, WindowUntil: opened.WindowUntil,
					Outcome:   investigation.RunFailed,
					Error:     "not executed: an external read requires a purpose",
					StartedAt: now, FinishedAt: now,
				}
			} else {
				identity := callIdentityOf(call)
				switch {
				case state.executedIdentities[identity] != 0:
					run = suppressedRun(opened, call, len(state.runs)+1,
						state.executedIdentities[identity])
					r.announce(ctx, events,
						investigation.ProgressPayload(
							"Skipped a repeat of "+offeredName(offered, call.Tool)+
								"; the earlier read already answers it"))
				case state.executed >= state.maxRuns:
					run = droppedRun(opened, call, len(state.runs)+1, fmt.Sprintf(
						"not executed: the investigation's read budget of %d was exhausted",
						state.maxRuns))
					r.announce(ctx, events,
						investigation.ProgressPayload(
							"Did not run "+offeredName(offered, call.Tool)+
								"; the read budget is exhausted"))
				default:
					ordinal := len(state.runs) + 1
					r.announceToolStarted(ctx, state, call, ordinal)
					var executeErr error
					run, executeErr = r.execute(ctx, opened, selections(offered),
						state.credentials, origin, call, ordinal)
					if executeErr != nil {
						terminalErr := failRun(failureReason(executeErr), state.usage)
						r.RuntimeTelemetry.Ended(time.Since(startedAt),
							investigation.StatusFailed.String(), "")
						return terminalErr
					}
					r.announce(ctx, events,
						investigation.ToolCompletedPayload(run))
					r.RuntimeTelemetry.RanTool(run)
					state.executed++
					fresh = run.Outcome == investigation.RunSucceeded
					executedRead = true
				}
				run.Purpose = boundText(call.Purpose, eventTextBound)
				if executedRead {
					state.executedIdentities[identity] = run.Ordinal
				}
			}
			if recordErr := r.recordToolRun(ctx, state, run); recordErr != nil {
				terminalErr := failRun(failureReason(recordErr), state.usage)
				r.RuntimeTelemetry.Ended(time.Since(startedAt),
					investigation.StatusFailed.String(), "")
				return terminalErr
			}
			result.Run = run
			freshRead = freshRead || fresh
			results = append(results, result)
		}
		if freshRead {
			stagnant = 0
		} else {
			stagnant++
		}
	}
}

func modelPrompt(r *Agent, state *runState, forced bool) Prompt {
	prompt := Prompt{
		Model: r.modelConfig.Model,
		System: []Block{
			{Text: safetyPolicy, Cache: true},
			{Text: state.task, Cache: true},
		},
		Content:         []Block{{Text: state.orientationText, Cache: true}},
		Tools:           state.tools,
		Turns:           state.transcript,
		MaxOutputTokens: r.modelConfig.MaxOutputTokens,
		Effort:          r.modelConfig.Effort,
	}
	if state.opening != "" {
		prompt.Content = append(prompt.Content, Block{Text: state.opening})
	}
	if forced {
		prompt.Tools = state.tools[len(state.tools)-1:]
		prompt.ForceTool = ConcludeToolName
	}
	return prompt
}

func decodeHypothesisSnapshot(
	arguments map[string]any, runs int,
) ([]investigation.HypothesisResult, error) {
	document, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encoding the hypothesis snapshot: %w", err)
	}
	var input struct {
		Hypotheses []struct {
			ID        string                         `json:"id"`
			Statement string                         `json:"statement"`
			Status    investigation.HypothesisStatus `json:"status"`
			Test      string                         `json:"test"`
			RunRefs   []int                          `json:"run_refs"`
		} `json:"hypotheses"`
	}
	if err := json.Unmarshal(document, &input); err != nil {
		return nil, fmt.Errorf("the hypothesis snapshot is not the declared document: %w", err)
	}
	if len(input.Hypotheses) > investigation.MaxHypothesisSnapshotItems {
		return nil, fmt.Errorf("the hypothesis snapshot has %d items; the limit is %d",
			len(input.Hypotheses), investigation.MaxHypothesisSnapshotItems)
	}
	seen := make(map[string]bool, len(input.Hypotheses))
	result := make([]investigation.HypothesisResult, 0, len(input.Hypotheses))
	for _, hypothesis := range input.Hypotheses {
		if strings.TrimSpace(hypothesis.ID) == "" || strings.TrimSpace(hypothesis.Statement) == "" ||
			strings.TrimSpace(hypothesis.Test) == "" {
			return nil, errors.New("each hypothesis requires id, statement, and test")
		}
		if seen[hypothesis.ID] {
			return nil, fmt.Errorf("hypothesis id %q appears more than once", hypothesis.ID)
		}
		if len([]rune(hypothesis.ID)) > 128 {
			return nil, errors.New("hypothesis id exceeds 128 characters")
		}
		seen[hypothesis.ID] = true
		if !hypothesisStatusAllowed(hypothesis.Status) {
			return nil, fmt.Errorf("hypothesis %q has invalid status %q", hypothesis.ID,
				hypothesis.Status)
		}
		references := make(map[int]bool, len(hypothesis.RunRefs))
		for _, run := range hypothesis.RunRefs {
			if references[run] {
				return nil, fmt.Errorf("hypothesis %q cites run %d more than once", hypothesis.ID, run)
			}
			references[run] = true
			if run < 1 || run > runs {
				return nil, fmt.Errorf("hypothesis %q cites run %d, but only %d runs exist",
					hypothesis.ID, run, runs)
			}
		}
		result = append(result, investigation.HypothesisResult{
			ID:        boundText(hypothesis.ID, eventTextBound),
			Statement: boundText(hypothesis.Statement, eventTextBound), Status: hypothesis.Status,
			Test: boundText(hypothesis.Test, eventTextBound), RunRefs: append([]int{}, hypothesis.RunRefs...),
		})
	}
	return result, nil
}

func hypothesisStatusAllowed(status investigation.HypothesisStatus) bool {
	for _, allowed := range investigation.HypothesisStatuses {
		if string(status) == allowed {
			return true
		}
	}
	return false
}

func offeredName(offeredSources []offeredSource, tool string) string {
	if _, _, offered := toolNamed(selections(offeredSources), tool); offered {
		return tool
	}
	return "a tool that is not offered"
}

func (r *Agent) announceToolStarted(
	ctx context.Context, state *runState, call toolCall, ordinal int,
) {
	source, _, offered := toolNamed(selections(state.offered), call.Tool)
	if !offered {
		return
	}
	payload := investigation.ToolStartedPayload(investigation.ToolRun{
		Ordinal: ordinal, Tool: call.Tool, Purpose: call.Purpose,
	}, source.integration.ID.String(), source.integration.Name)
	r.announce(ctx, state.events, payload)
}

func (r *Agent) recordToolRun(
	ctx context.Context, state *runState, run investigation.ToolRun,
) error {
	state.runs = append(state.runs, run)
	if run.Ordinal > state.highestOrdinal {
		state.highestOrdinal = run.Ordinal
	}
	if err := r.Store.RecordToolRun(
		ctx, state.organization, state.opened.ID, state.opened.ClaimToken, run); err != nil {
		return errProvenance
	}
	return nil
}

func failureReason(err error) string {
	if errors.Is(err, errProvenance) || errors.Is(err, errNoConclusion) {
		return err.Error()
	}
	return reasonerFailure(err)
}

func offeredSources(
	catalog integrations.Catalog, candidates []integrations.Integration,
) []offeredSource {
	var sources []offeredSource
	for _, candidate := range candidates {
		definition, known := catalog.Lookup(candidate.Provider)
		if !known {
			continue
		}
		tools := offeredTools(definition, candidate)
		if len(tools) == 0 {
			continue
		}
		sources = append(sources, offeredSource{Integration: candidate, Tools: tools})
	}
	bindDuplicateToolNames(sources)
	sortSourcesByName(sources)
	return sources
}

func offeredSourcesForConversation(
	catalog integrations.Catalog,
	candidates []integrations.Integration,
	origin *investigation.ConversationOrigin,
) []offeredSource {
	if origin == nil {
		return offeredSources(catalog, candidates)
	}
	if origin.IntegrationID == uuid.Nil || origin.Channel == "" || origin.Thread == "" {
		return nil
	}

	var originProvider integrations.Provider
	found := false
	for _, candidate := range candidates {
		if candidate.ID == origin.IntegrationID {
			originProvider = candidate.Provider
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	allowed := make([]integrations.Integration, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Provider != originProvider || candidate.ID == origin.IntegrationID {
			allowed = append(allowed, candidate)
		}
	}

	var scoped []offeredSource
	for _, source := range offeredSources(catalog, allowed) {
		if source.Integration.Provider != originProvider {
			scoped = append(scoped, source)
			continue
		}
		var threadScopedTools []integrations.Tool
		for _, tool := range source.Tools {
			if tool.SupportsThreadScope {
				threadScopedTools = append(threadScopedTools, tool)
			}
		}
		if len(threadScopedTools) > 0 {
			source.Tools = threadScopedTools
			scoped = append(scoped, source)
		}
	}
	return scoped
}

func bindDuplicateToolNames(sources []offeredSource) {
	counts := map[string]int{}
	for _, source := range sources {
		for _, tool := range source.Tools {
			counts[tool.Name]++
		}
	}
	for sourceIndex := range sources {
		for toolIndex := range sources[sourceIndex].Tools {
			tool := &sources[sourceIndex].Tools[toolIndex]
			if counts[tool.Name] > 1 {
				tool.Name += "__" + strings.ReplaceAll(sources[sourceIndex].Integration.ID.String(), "-", "")
			}
		}
	}
}

func suppressedRun(
	opened investigation.Investigation, call toolCall, ordinal, original int,
) investigation.ToolRun {
	now := time.Now().UTC()
	return investigation.ToolRun{
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Outcome:     investigation.RunFailed,
		Error: fmt.Sprintf("not executed: identical to run %d, whose result is already "+
			"above; call a different tool, or the same tool with different arguments, "+
			"to gather new evidence — or conclude", original),
		StartedAt:  now,
		FinishedAt: now,
	}
}

func callIdentityOf(call toolCall) string {
	encoded, err := json.Marshal(call.Arguments)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", call.Arguments))
	}
	return call.Tool + " " + string(encoded)
}

func concludeReason(stoppedBy string, offered int) string {
	if offered == 0 {
		return "No readable sources are connected. Conclude from the subject alone."
	}
	switch stoppedBy {
	case investigation.StoppedByToolRuns:
		return "The investigation's read budget was exhausted."
	case investigation.StoppedByReasonerTurns:
		return "The investigation's turn budget was exhausted."
	case investigation.StoppedByWallClock:
		return "The investigation's time is nearly over."
	case investigation.StoppedByContext:
		return "This turn has filled the working context available to it."
	default:
		return ""
	}
}

func wallClockAlmostOver(ctx context.Context, reserve time.Duration) bool {
	deadline, has := ctx.Deadline()
	return has && time.Until(deadline) < reserve
}

func boundActions(actions []investigation.ActionProposal) []investigation.ActionProposal {
	if len(actions) > investigation.MaxConclusionActions {
		actions = actions[:investigation.MaxConclusionActions]
	}
	kept := make([]investigation.ActionProposal, 0, len(actions))
	for _, action := range actions {
		action.Title = boundText(action.Title, investigation.MaxActionTextLength)
		action.Rationale = boundText(action.Rationale, investigation.MaxActionTextLength)
		action.Verification = boundText(action.Verification, investigation.MaxActionTextLength)
		kept = append(kept, action)
	}
	return kept
}

func (r *Agent) orientation(
	ctx context.Context,
	organization uuid.UUID,
	opened investigation.Investigation,
	offered []offeredSource,
	brief *investigation.Brief,
) orientation {
	oriented := orientation{
		Subject:     opened.Subject,
		Question:    opened.Question,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Sources:     offered,
		Brief:       brief,
	}
	if opened.IncidentID != uuid.Nil {
		if trigger, err := r.Store.TriggerIncident(ctx, organization, opened.IncidentID); err == nil {
			oriented.Trigger = &trigger
		}
	}
	if inventory, err := r.Store.WorkloadInventory(
		ctx, organization, inventoryDigestLimit); err == nil {
		oriented.Inventory = inventory
	}
	return oriented
}

func selections(offered []offeredSource) []selection {
	selections := make([]selection, 0, len(offered))
	for _, source := range offered {
		selections = append(selections, selection{
			integration: source.Integration, tools: source.Tools,
		})
	}
	return selections
}

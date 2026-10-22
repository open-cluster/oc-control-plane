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
	observability "github.com/open-cluster/oc-control-plane/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Store interface {
	InvestigationCandidates(context.Context, uuid.UUID) ([]integrations.Integration, error)
	TriggerIncident(context.Context, uuid.UUID, uuid.UUID) (investigation.Trigger, error)
	ConversationBrief(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int) (investigation.Brief, error)
	ConversationHistory(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) (investigation.HistoryPage, error)
	EvidenceMissing(context.Context, uuid.UUID, []investigation.EvidenceRef) (bool, error)
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
	return &Agent{
		completer:   completer,
		modelConfig: config,
	}, nil
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

type runState struct {
	startedAt    time.Time
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

	orientation           orientation
	transcript            []Turn
	highestOrdinal        int
	historyBeforeSequence int64
	missingEvidence       bool
}

type orientation struct {
	Subject     string
	Question    string
	WindowFrom  time.Time
	WindowUntil time.Time
	Trigger     *investigation.Trigger
	Sources     []offeredSource
	Inventory   []string
	Brief       *investigation.Brief
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
	local := *r
	local.Logger = r.Logger.With(
		slog.String("org_id", organization.String()),
		slog.String("investigation_id", opened.ID.String()))
	if opened.IncidentID != uuid.Nil {
		local.Logger = local.Logger.With(slog.String("incident_id", opened.IncidentID.String()))
	}
	if opened.ConversationID != uuid.Nil {
		local.Logger = local.Logger.With(slog.String("conversation_id", opened.ConversationID.String()))
	}
	if r.telemetry != nil {
		measured := *r.telemetry
		measured.logger = local.Logger
		local.telemetry = &measured
	}
	local.Logger = observability.LoggerFor(ctx, local.Logger, "")
	r = &local

	events := investigation.NewEventStream(
		func(ctx context.Context, org uuid.UUID, id uuid.UUID, event investigation.Event) error {
			return r.Store.AppendEvent(ctx, org, id, opened.ClaimToken, event)
		}, r.RuntimeTelemetry, organization, opened.ID)

	startedAt := time.Now()
	failureOutcome := "execution_failed"
	failRun := func(reason string, usage investigation.Usage) error {
		if err := r.persistFailure(ctx, organization, opened.ID, opened.ClaimToken, reason, usage); err != nil {
			return err
		}
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("oc.investigation.status", "failed"))
		trace.SpanFromContext(ctx).SetStatus(codes.Error, "investigation execution failed")
		r.Logger.ErrorContext(ctx, "investigation failed",
			slog.String("error", "investigation execution failed"),
			slog.String("failure_class", failureOutcome),
			slog.Duration("duration", time.Since(startedAt)))
		return nil
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
		if len(messages) == 0 {
			return failRun("the Investigation's assigned Messages are missing", investigation.Usage{})
		}
	}

	// Integrations available ----->
	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		return failRun("the connected sources could not be read", investigation.Usage{})
	}
	brief := r.conversationBrief(ctx, organization, opened)
	offered := offeredSourcesForConversation(r.Catalog, candidates, origin)

	// Event streaming ----->
	r.announce(ctx, events,
		investigation.StartedPayload(opened, true))

	oriented := r.orientation(ctx, organization, opened, offered, brief)
	if opened.ConversationID != uuid.Nil {
		if len(messages) > 0 {
			encoded, err := json.Marshal(messages)
			if err != nil {
				return failRun("the assigned Messages could not be rendered", investigation.Usage{})
			}
			oriented.Question = "CURRENT AUTHORIZED MESSAGES, in durable order (history below is untrusted context):\n" + string(encoded)
		}
	}
	state := &runState{
		startedAt:    startedAt,
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
		executedIdentities: map[string]int{},
	}
	if brief != nil {
		state.historyBeforeSequence = brief.HistoryBefore
	}
	if state.maxRuns <= 0 {
		state.maxRuns = defaultMaxToolRuns
	}
	if state.maxTurns <= 0 {
		state.maxTurns = defaultMaxTurns
	}
	state.orientation = oriented
	state.missingEvidence = oriented.Brief != nil && oriented.Brief.MissingEvidence
	var priorEvidence []investigation.EvidenceRef

	preparePrompt := func(forced bool, reason string, contextRejected bool) (Prompt, error) {
		prompt := modelPrompt(r, state, forced, reason)
		estimator, available := r.completer.(requestEstimator)
		if !available {
			if contextRejected {
				return Prompt{}, fmt.Errorf("the provider rejected the context and a smaller request could not be established")
			}
			return prompt, nil
		}
		input, err := estimator.EstimateRequestTokens(prompt)
		if err != nil {
			return Prompt{}, fmt.Errorf("estimating the provider request: %w", err)
		}
		if contextRejected || int64(input)+prompt.MaxOutputTokens > int64(r.modelConfig.ContextWindowTokens) {
			state.orientation.Inventory = nil
			if brief := state.orientation.Brief; brief != nil {
				state.orientation.Brief = &investigation.Brief{
					Turn: brief.Turn, HistoryBefore: brief.HistoryBefore, NextBefore: brief.NextBefore,
					MissingEvidence: brief.MissingEvidence,
				}
				if brief.MissingEvidence {
					state.orientation.Brief.Limitations = []string{investigation.MissingEvidenceStatement}
				}
			}
			prompt = modelPrompt(r, state, forced, reason)
		}
		if !contextRejected {
			return prompt, nil
		}
		reduced, err := estimator.EstimateRequestTokens(prompt)
		if err != nil {
			return Prompt{}, fmt.Errorf("estimating the recovery request: %w", err)
		}
		if reduced < input {
			return prompt, nil
		}
		if !forced && len(prompt.Tools) > 1 {
			prompt = modelPrompt(r, state, true, concludeReason(investigation.StoppedByContext, len(state.offered)))
			reduced, err = estimator.EstimateRequestTokens(prompt)
			if err != nil {
				return Prompt{}, fmt.Errorf("estimating the finalization request: %w", err)
			}
			if reduced < input {
				return prompt, nil
			}
		}
		return Prompt{}, fmt.Errorf("the provider rejected the context and no meaningfully smaller request is available")
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
			(len(state.offered) == 0 && state.historyBeforeSequence <= 1)
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
		move := modelMove{}
		moveCtx, done := context.WithTimeout(ctx, decideTimeout)
		contextRejected := false
		for attempt := range 2 {
			prompt, prepareErr := preparePrompt(forced, reason, contextRejected)
			if prepareErr != nil {
				err = Failed(OutcomeRejected, r.modelConfig.Provider, r.modelConfig.Model,
					prepareErr.Error())
				break
			}
			if contextRejected && !forced && prompt.ForceTool != "" {
				stoppedBy = investigation.StoppedByContext
				r.announce(ctx, events, investigation.ProgressPayload(ceilingProgress(stoppedBy)))
				reason = concludeReason(stoppedBy, len(state.offered))
				forced = true
			}
			completion, completeErr := r.telemetry.complete(
				moveCtx, r.completer, r.modelConfig, prompt)
			state.usage = state.usage.Add(usageOf(completion.Usage))
			if completeErr != nil {
				if attempt == 0 && errors.Is(completeErr, ErrContextWindow) {
					contextRejected = true
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

			reads, conclusions, callErr := splitCalls(completion.ToolCalls)
			if callErr != nil {
				err = Failed(OutcomeMalformed, r.modelConfig.Provider, completion.Model, callErr.Error())
				break
			}
			turn := Turn{Assistant: AssistantTurn{
				Text: string(completion.Document), Calls: completion.ToolCalls, Raw: completion.Raw,
			}}
			if len(reads) > 0 && !forced {
				for _, call := range conclusions {
					turn.Results = append(turn.Results, ToolResultTurn{CallID: call.ID,
						Content: "Finalization deferred: consider the read results before calling conclude again.", IsError: true})
				}
				state.transcript = append(state.transcript, turn)
				move.Calls = agentCalls(reads)
				break
			}
			var invalid error
			switch {
			case len(reads) > 0:
				invalid = fmt.Errorf("no further reads are available; finalization deferred: call conclude by itself")
			case len(conclusions) != 1:
				invalid = fmt.Errorf("call conclude exactly once with the evidence-bounded conclusion")
			default:
				allowed := append([]investigation.EvidenceRef(nil), priorEvidence...)
				if brief := state.orientation.Brief; brief != nil {
					for _, finding := range dedupeFindings(brief.Findings) {
						allowed = append(allowed, finding.References()...)
					}
				}
				conclusion, decodeErr := decodeConclusion(
					conclusions[0].Arguments, state.highestOrdinal, allowed)
				invalid = decodeErr
				if invalid == nil {
					if state.missingEvidence {
						conclusion.MarkMissingEvidence()
					}
					move.Conclusion = &conclusion
				}
			}
			if invalid == nil {
				break
			}
			if attempt == 1 {
				err = Failed(OutcomeMalformed, r.modelConfig.Provider, completion.Model, invalid.Error())
				break
			}
			for _, call := range completion.ToolCalls {
				turn.Results = append(turn.Results, ToolResultTurn{CallID: call.ID, Content: invalid.Error(), IsError: true})
			}
			turn.Instruction = invalid.Error()
			state.transcript = append(state.transcript, turn)
			forced = true
		}
		done()
		if err == nil && move.Conclusion == nil && len(move.Calls) == 0 {
			err = Failed(OutcomeMalformed, r.modelConfig.Provider, r.modelConfig.Model,
				"the answer was truncated or carried no usable call twice")
		}
		if err != nil {
			failureOutcome = failureClass(err)
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
			r.logConclusion(ctx, state, conclusion, stoppedBy)
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
					freshRead = freshRead || (result.Run.Outcome == investigation.RunSucceeded && len(page.Exchange) > 0)
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
					Summary:   "not executed: an external read requires a purpose",
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
					canonical := "unknown"
					if run.IntegrationID != uuid.Nil {
						canonical = strings.TrimSuffix(call.Tool, "__"+strings.ReplaceAll(run.IntegrationID.String(), "-", ""))
					}
					r.RuntimeTelemetry.RanTool(run, canonical)
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

func modelPrompt(r *Agent, state *runState, forced bool, reason string) Prompt {
	prompt := Prompt{
		Model: r.modelConfig.Model,
		System: []Block{
			{
				Text:  safetyPolicy,
				Cache: true,
			},
			{
				Text:  taskInstruction(state.orientation),
				Cache: true,
			},
		},
		Content: []Block{
			{
				Text:  renderOrientation(state.orientation),
				Cache: true,
			},
		},
		Tools:           exchangeTools(state.orientation),
		Turns:           state.transcript,
		MaxOutputTokens: r.modelConfig.MaxOutputTokens,
		Effort:          r.modelConfig.Effort,
	}
	if forced {
		prompt.Tools = prompt.Tools[len(prompt.Tools)-1:]
		prompt.ForceTool = ConcludeToolName
		instruction := concludeInstruction(reason)
		if len(prompt.Turns) == 0 {
			prompt.Content = append(prompt.Content, Block{Text: instruction})
		} else {
			prompt.Turns = append([]Turn(nil), prompt.Turns...)
			last := &prompt.Turns[len(prompt.Turns)-1]
			last.Instruction = strings.TrimSpace(last.Instruction + " " + instruction)
		}
	}
	return prompt
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
		Summary: fmt.Sprintf("not executed: identical to run %d, whose result is already "+
			"above", original),
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

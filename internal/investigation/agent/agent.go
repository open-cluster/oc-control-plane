// Package agent runs an Investigation through model completions and read-only Tools.
// Provider adapters remain in subpackages, and customer evidence is never logged or traced.
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

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// Store is the durable state Agent.Run consumes.
type Store interface {
	InvestigationCandidates(context.Context, tenancy.Organization) ([]integrations.Integration, error)
	TriggerIncident(context.Context, tenancy.Organization, uuid.UUID) (investigation.Trigger, error)
	ConversationBrief(context.Context, tenancy.Organization, uuid.UUID, int) (investigation.Brief, error)
	WorkloadInventory(context.Context, tenancy.Organization, int) ([]string, error)
	RecordToolRun(context.Context, tenancy.Organization, uuid.UUID, investigation.ToolRun) error
	RecordCredentialUnseal(context.Context, tenancy.Organization, uuid.UUID, string) error
	AppendEvent(context.Context, tenancy.Organization, uuid.UUID, investigation.Event) error
	ConcludeInvestigation(context.Context, tenancy.Organization, uuid.UUID,
		investigation.Conclusion, string, investigation.Usage) error
	FailInvestigation(context.Context, tenancy.Organization, uuid.UUID, string, investigation.Usage) error
}

// Agent runs investigations against one validated model deployment.
type Agent struct {
	model      Model
	deployment Deployment
	telemetry  *Telemetry

	Store               Store
	Catalog             integrations.Catalog
	Sealer              seal.Sealer
	RuntimeTelemetry    *investigation.Telemetry
	Logger              *slog.Logger
	MaxToolRuns         int
	MaxTurns            int
	ContextWindowTokens int
}

// NewAgent binds one validated model deployment to the Investigation runtime.
func NewAgent(deployment Deployment, model Model) (*Agent, error) {
	return &Agent{model: model, deployment: deployment}, nil
}

// Instrument attaches the per-call telemetry. Without it the agent still works and
// emits nothing, which is only right for tests.
func (a *Agent) Instrument(telemetry *Telemetry) { a.telemetry = telemetry }

// Agent-level bounds protect runtime and context.
const (
	// defaultMaxToolRuns bounds the whole investigation's reads.
	defaultMaxToolRuns = 30
	// defaultMaxTurns bounds model turns before a forced conclusion.
	defaultMaxTurns = 20
	// maxStagnantTurns is how many consecutive turns may produce no new evidence before
	// the conclusion is forced. A turn with a fresh successful read resets the count.
	maxStagnantTurns = 2
	// wallClockReserve is how much of the investigation's deadline is kept back for the
	// concluding turn: a run nearing its deadline concludes with
	// what it has instead of dying mid-read as a failure.
	wallClockReserve = 2 * time.Minute
	// inventoryDigestLimit bounds the orientation's workload digest.
	inventoryDigestLimit = 50
	eventTextBound       = 512
	decideTimeout        = 6 * time.Minute
	defaultContextWindow = 128_000
)

// errProvenance marks a run that could not be recorded. It aborts the investigation
// wherever it surfaces — a read whose record failed must not inform a conclusion.
var errProvenance = errors.New("a tool run could not be recorded")

// errNoConclusion marks a model that ignored a forced conclusion.
var errNoConclusion = errors.New("the reasoner did not conclude when required to")

const UpdateHypothesesToolName = "update_hypotheses"

// runState is the private data carried by Agent.Run. It has no behavior so the state
// machine remains visible in Run.
type runState struct {
	organization tenancy.Organization
	opened       investigation.Investigation
	offered      []offeredSource
	brief        *investigation.Brief
	events       *investigation.EventStream
	credentials  *credentialCache
	maxRuns      int
	maxTurns     int
	ceiling      int
	carried      int

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
}

type orientation struct {
	Subject     string
	Question    string
	WindowFrom  time.Time
	WindowUntil time.Time
	Trigger     *investigation.Trigger
	Sources     []offeredSource
	Inventory   []string
	Preflight   []investigation.ToolRun
	Brief       *investigation.Brief
}

type offeredSource struct {
	Integration integrations.Integration
	Tools       []integrations.Tool
}

type toolCall struct {
	ID           string
	Tool         string
	Purpose      string
	HypothesisID string
	Arguments    map[string]any
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

// Run performs one investigation through a durable terminal result.
func (r *Agent) Run(
	ctx context.Context, organization tenancy.Organization, opened investigation.Investigation,
) error {
	events := investigation.NewEventStream(
		r.Store.AppendEvent, r.RuntimeTelemetry, organization, opened.ID,
	)
	startedAt := time.Now()
	failRun := func(reason string, usage investigation.Usage) error {
		safeReason, err := r.persistFailure(ctx, organization, opened.ID, reason, usage)
		if err != nil {
			return err
		}
		writeCtx, done := terminalWriteWindow(ctx)
		defer done()
		r.announce(writeCtx, events, investigation.EventFailed,
			investigation.FailedPayload(safeReason))
		return nil
	}

	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		return failRun("the connected sources could not be read", investigation.Usage{})
	}
	brief := r.conversationBrief(ctx, organization, opened, events)
	offered := offeredSourcesForConversation(r.Catalog, candidates, brief)
	if opened.ConversationID != uuid.Nil && brief == nil {
		var safe []offeredSource
		for _, source := range offered {
			conversationProvider := false
			for _, tool := range source.Tools {
				if tool.ConversationScoped {
					conversationProvider = true
					break
				}
			}
			if !conversationProvider {
				safe = append(safe, source)
			}
		}
		offered = safe
	}
	r.announce(ctx, events, investigation.EventStarted,
		investigation.StartedPayload(opened, true))

	oriented := r.orientation(ctx, organization, opened, offered, brief)
	contextWindow := r.ContextWindowTokens
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	state := &runState{
		organization: organization,
		opened:       opened,
		offered:      offered,
		brief:        brief,
		events:       events,
		credentials: newCredentialCache(r.Sealer, func(ctx context.Context, id uuid.UUID) error {
			return r.Store.RecordCredentialUnseal(ctx, organization, id,
				"investigation "+opened.ID.String())
		}),
		maxRuns:            r.MaxToolRuns,
		maxTurns:           r.MaxTurns,
		ceiling:            contextWindow - int(r.deployment.MaxOutputTokens),
		executedIdentities: map[string]int{},
	}
	if state.maxRuns <= 0 {
		state.maxRuns = defaultMaxToolRuns
	}
	if state.maxTurns <= 0 {
		state.maxTurns = defaultMaxTurns
	}
	for _, call := range preflightCalls(oriented) {
		identity := callIdentityOf(call)
		var run investigation.ToolRun
		executedRead := false
		switch {
		case state.executedIdentities[identity] != 0:
			run = suppressedRun(opened, call, len(state.runs)+1,
				state.executedIdentities[identity])
			r.announce(ctx, events, investigation.EventProgress, investigation.ProgressPayload(
				"Skipped a repeat of "+offeredName(offered, call.Tool)+
					"; the earlier read already answers it"))
		case state.executed >= state.maxRuns:
			run = droppedRun(opened,
				investigation.ToolCall{Tool: call.Tool, Arguments: call.Arguments},
				len(state.runs)+1, fmt.Sprintf(
					"not executed: the investigation's read budget of %d was exhausted",
					state.maxRuns))
			r.announce(ctx, events, investigation.EventProgress, investigation.ProgressPayload(
				"Did not run "+offeredName(offered, call.Tool)+"; the read budget is exhausted"))
		default:
			ordinal := len(state.runs) + 1
			r.announceToolStarted(ctx, state, call, ordinal)
			var executeErr error
			run, executeErr = r.execute(ctx, opened, selections(offered),
				state.credentials, brief,
				investigation.ToolCall{Tool: call.Tool, Arguments: call.Arguments}, ordinal)
			if executeErr != nil {
				return failRun("preflight provenance could not be recorded", state.usage)
			}
			r.announce(ctx, events, investigation.EventToolCompleted,
				investigation.ToolCompletedPayload(run))
			r.RuntimeTelemetry.RanTool(run)
			state.executed++
			executedRead = true
		}
		run.Purpose = boundText(call.Purpose, eventTextBound)
		run.HypothesisID = boundText(call.HypothesisID, eventTextBound)
		if preflightErr := r.recordToolRun(ctx, state, run); preflightErr != nil {
			return failRun("preflight provenance could not be recorded", state.usage)
		}
		if executedRead {
			state.executedIdentities[identity] = run.Ordinal
		}
		oriented.Preflight = append(oriented.Preflight, run)
	}
	state.task = taskInstruction(oriented)
	state.orientationText = renderOrientation(oriented)
	state.tools = exchangeTools(oriented)
	state.carried = orientationTokens(oriented)

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
		if stoppedBy == "" {
			switch {
			case state.executed >= state.maxRuns:
				stoppedBy = investigation.StoppedByToolRuns
			case turn > state.maxTurns:
				stoppedBy = investigation.StoppedByReasonerTurns
			case wallClockAlmostOver(ctx, wallClockReserve):
				stoppedBy = investigation.StoppedByWallClock
			case stagnant >= maxStagnantTurns:
				stoppedBy = investigation.StoppedByStagnation
			case state.carried >= state.ceiling:
				stoppedBy = investigation.StoppedByContext
			}
			if stoppedBy != "" {
				r.announce(ctx, events, investigation.EventProgress,
					investigation.ProgressPayload(ceilingProgress(stoppedBy)))
			}
		}

		mustConclude := stoppedBy != "" || len(state.offered) == 0
		reason := concludeReason(stoppedBy, len(state.offered))
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
		if forced {
			instruction := concludeInstruction(reason)
			if len(state.transcript) == 0 {
				state.opening = instruction
			} else {
				state.transcript[len(state.transcript)-1].Instruction = instruction
			}
		}

		move := modelMove{}
		moveCtx, done := context.WithTimeout(ctx, decideTimeout)
		for attempt := range 2 {
			completion, completeErr := r.telemetry.complete(
				moveCtx, r.model, r.deployment, modelPrompt(r, state, forced))
			state.usage = state.usage.Add(usageOf(completion.Usage))
			if completeErr != nil {
				err = completeErr
				break
			}

			switch completion.Stop {
			case StopRefused:
				err = Failed(OutcomeRefused, r.deployment.Provider,
					completion.Model, "the provider's safeguards declined the investigation")
			case StopTruncated:
				continue
			}
			if err != nil {
				break
			}

			reads, conclude := splitCalls(completion.ToolCalls)
			if len(reads) > 0 && !mustConclude {
				state.transcript = append(state.transcript, Turn{Assistant: AssistantTurn{
					Text: string(completion.Document), Calls: completion.ToolCalls, Raw: completion.Raw,
				}})
				move.Calls = agentCalls(reads)
				break
			}
			if conclude != nil {
				conclusion, decodeErr := decodeConclusion(
					conclude.Arguments, state.highestOrdinal, false)
				if decodeErr != nil {
					if attempt == 0 {
						continue
					}
					err = Failed(OutcomeMalformed, r.deployment.Provider,
						completion.Model, decodeErr.Error())
					break
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
			err = Failed(OutcomeMalformed, r.deployment.Provider, r.deployment.Model,
				"the answer was truncated or carried no usable call twice")
		}
		done()
		if err == nil && move.Conclusion == nil && len(move.Calls) == 0 {
			err = Failed(OutcomeMalformed, r.deployment.Provider, r.deployment.Model,
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
			if err := r.Store.ConcludeInvestigation(writeCtx, organization, opened.ID,
				conclusion, stoppedBy, state.usage); err != nil {
				done()
				return fmt.Errorf("recording investigation conclusion: %w", err)
			}
			r.announce(writeCtx, events, investigation.EventConcluded,
				investigation.ConcludedPayload(conclusion, stoppedBy))
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
		if mustConclude {
			terminalErr := failRun(errNoConclusion.Error(), state.usage)
			r.RuntimeTelemetry.Ended(time.Since(startedAt), investigation.StatusFailed.String(), "")
			return terminalErr
		}

		results = make([]toolFeedback, 0, len(move.Calls))
		freshRead := false
		for _, call := range move.Calls {
			result := toolFeedback{CallID: call.ID}
			fresh := false
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
					r.announce(ctx, events, investigation.EventHypothesesUpdated,
						investigation.HypothesesUpdatedPayload(snapshot))
				}
				state.carried += runTokens(result.Run)
				results = append(results, result)
				continue
			}

			var run investigation.ToolRun
			executedRead := false
			if strings.TrimSpace(call.Purpose) == "" {
				now := time.Now().UTC()
				run = investigation.ToolRun{
					Ordinal: len(state.runs) + 1, Tool: call.Tool, Arguments: call.Arguments,
					HypothesisID: boundText(call.HypothesisID, eventTextBound),
					WindowFrom:   opened.WindowFrom, WindowUntil: opened.WindowUntil,
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
					r.announce(ctx, events, investigation.EventProgress,
						investigation.ProgressPayload(
							"Skipped a repeat of "+offeredName(offered, call.Tool)+
								"; the earlier read already answers it"))
				case state.executed >= state.maxRuns:
					run = droppedRun(opened,
						investigation.ToolCall{Tool: call.Tool, Arguments: call.Arguments},
						len(state.runs)+1, fmt.Sprintf(
							"not executed: the investigation's read budget of %d was exhausted",
							state.maxRuns))
					r.announce(ctx, events, investigation.EventProgress,
						investigation.ProgressPayload(
							"Did not run "+offeredName(offered, call.Tool)+
								"; the read budget is exhausted"))
				default:
					ordinal := len(state.runs) + 1
					r.announceToolStarted(ctx, state, call, ordinal)
					var executeErr error
					run, executeErr = r.execute(ctx, opened, selections(offered),
						state.credentials, brief,
						investigation.ToolCall{Tool: call.Tool, Arguments: call.Arguments},
						ordinal)
					if executeErr != nil {
						terminalErr := failRun(failureReason(executeErr), state.usage)
						r.RuntimeTelemetry.Ended(time.Since(startedAt),
							investigation.StatusFailed.String(), "")
						return terminalErr
					}
					r.announce(ctx, events, investigation.EventToolCompleted,
						investigation.ToolCompletedPayload(run))
					r.RuntimeTelemetry.RanTool(run)
					state.executed++
					fresh = run.Outcome == investigation.RunSucceeded
					executedRead = true
				}
				run.Purpose = boundText(call.Purpose, eventTextBound)
				run.HypothesisID = boundText(call.HypothesisID, eventTextBound)
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
			state.carried += runTokens(result.Run)
			results = append(results, result)
		}
		if freshRead {
			stagnant = 0
		} else {
			stagnant++
		}
	}
}

// modelPrompt renders the immutable orientation and the transcript Agent.Run owns.
func modelPrompt(r *Agent, state *runState, forced bool) Prompt {
	prompt := Prompt{
		Model: r.deployment.Model,
		System: []Block{
			{Text: safetyPolicy, Cache: true},
			{Text: state.task, Cache: true},
		},
		Content:         []Block{{Text: state.orientationText, Cache: true}},
		Tools:           state.tools,
		Turns:           state.transcript,
		MaxOutputTokens: r.deployment.MaxOutputTokens,
		Effort:          r.deployment.Effort,
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

func preflightCalls(oriented orientation) []toolCall {
	if oriented.Trigger == nil || oriented.Brief != nil {
		return nil
	}
	labels := oriented.Trigger.Labels
	namespace := labels["namespace"]
	workloadKind := labels["workload_kind"]
	workloadName := labels["workload_name"]
	exactNamespace := exactKubernetesIdentifier(namespace)
	exactWorkload := exactNamespace && exactKubernetesIdentifier(workloadName) &&
		(workloadKind == "Deployment" || workloadKind == "StatefulSet" ||
			workloadKind == "DaemonSet")

	var runtime, events []toolCall
	for _, source := range oriented.Sources {
		if source.Integration.Type != integrations.TypeKubernetes {
			continue
		}
		for _, tool := range source.Tools {
			base := strings.SplitN(tool.Name, "__", 2)[0]
			switch {
			case base == "kubernetes.workload.runtime" && exactWorkload:
				runtime = append(runtime, toolCall{
					ID: "preflight-runtime-" + source.Integration.ID.String(), Tool: tool.Name,
					Purpose: "establish the exact workload's current runtime state",
					Arguments: map[string]any{"namespace": namespace,
						"workloadKind": workloadKind, "workloadName": workloadName},
				})
			case base == "kubernetes.namespace.events" && exactNamespace:
				events = append(events, toolCall{
					ID: "preflight-events-" + source.Integration.ID.String(), Tool: tool.Name,
					Purpose:   "read recent events in the exact alert namespace",
					Arguments: map[string]any{"namespace": namespace},
				})
			}
		}
	}
	return append(runtime, events...)
}

func exactKubernetesIdentifier(value string) bool {
	if value == "" || len(value) > 253 || value != strings.ToLower(value) ||
		value[0] < 'a' || value[0] > 'z' {
		return false
	}
	last := value[len(value)-1]
	if (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
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
		seen[hypothesis.ID] = true
		if !hypothesisStatusAllowed(hypothesis.Status) {
			return nil, fmt.Errorf("hypothesis %q has invalid status %q", hypothesis.ID,
				hypothesis.Status)
		}
		for _, run := range hypothesis.RunRefs {
			if run < 1 || run > runs {
				return nil, fmt.Errorf("hypothesis %q cites run %d, but only %d runs exist",
					hypothesis.ID, run, runs)
			}
		}
		result = append(result, investigation.HypothesisResult{
			ID:        boundText(hypothesis.ID, eventTextBound),
			Statement: boundText(hypothesis.Statement, eventTextBound), Status: hypothesis.Status,
			Test: boundText(hypothesis.Test, eventTextBound), RunRefs: hypothesis.RunRefs,
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

// offeredName renders a tool name for a PROGRESS line, which is prose the platform writes.
//
// A tool name arrives from the model, and a model can invent one — the run then fails with
// "not one of the tools the selected sources offer". Interpolating it would put a string
// the model chose into a sentence the platform is supposed to have authored, which is the
// one thing this stream promises never to carry. So a name is only spoken when it is one
// this deployment actually offers; anything else is described rather than quoted.
func offeredName(offeredSources []offeredSource, tool string) string {
	if _, _, offered := toolNamed(selections(offeredSources), tool); offered {
		return tool
	}
	return "a tool that is not offered"
}

// announceToolStarted says which read is about to happen and where it is going, resolving
// the integration from the offered sources with the same lookup the execution itself uses,
// so the event names the source the read actually reaches.
func (r *Agent) announceToolStarted(
	ctx context.Context, state *runState, call toolCall, ordinal int,
) {
	integration, name := "", ""
	if source, _, offered := toolNamed(selections(state.offered), call.Tool); offered {
		integration = source.integration.ID.String()
		name = source.integration.Name
	}
	payload := investigation.ToolStartedPayload(investigation.ToolRun{
		Ordinal: ordinal, Tool: call.Tool, Purpose: call.Purpose,
		HypothesisID: call.HypothesisID, Arguments: call.Arguments,
	}, integration)
	if name != "" {
		payload["integration"] = name
	}
	r.announce(ctx, state.events, investigation.EventToolStarted, payload)
}

// record writes one run into the provenance, in ordinal order.
func (r *Agent) recordToolRun(
	ctx context.Context, state *runState, run investigation.ToolRun,
) error {
	state.runs = append(state.runs, run)
	if run.Ordinal > state.highestOrdinal {
		state.highestOrdinal = run.Ordinal
	}
	if err := r.Store.RecordToolRun(
		ctx, state.organization, state.opened.ID, run); err != nil {
		return errProvenance
	}
	return nil
}

// failureReason renders a model error as the recordable failure reason: the
// loop's own sentinels speak for themselves, anything else came from the model boundary.
func failureReason(err error) string {
	if errors.Is(err, errProvenance) || errors.Is(err, errNoConclusion) {
		return err.Error()
	}
	return reasonerFailure(err)
}

// offeredSources is every enabled candidate whose verified grants support at least one
// tool, in stable name order: the investigator's whole universe, derived from verified
// reality.
func offeredSources(
	catalog integrations.Catalog, candidates []integrations.Integration,
) []offeredSource {
	var sources []offeredSource
	for _, candidate := range candidates {
		definition, known := catalog.ByID(candidate.Type)
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

// offeredSourcesForConversation confines a provider-originated Conversation to its
// originating thread. Other provider categories remain available, while another
// installation of the originating provider and its broader reads are not implied.
func offeredSourcesForConversation(
	catalog integrations.Catalog, candidates []integrations.Integration, brief *investigation.Brief,
) []offeredSource {
	if brief == nil || brief.OriginIntegrationID == "" ||
		brief.OriginChannel == "" || brief.OriginThread == "" {
		return offeredSources(catalog, candidates)
	}

	var originType integrations.TypeID
	found := false
	for _, candidate := range candidates {
		if candidate.ID.String() == brief.OriginIntegrationID {
			originType = candidate.Type
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	allowed := make([]integrations.Integration, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Type != originType || candidate.ID.String() == brief.OriginIntegrationID {
			allowed = append(allowed, candidate)
		}
	}

	var scoped []offeredSource
	for _, source := range offeredSources(catalog, allowed) {
		if source.Integration.Type != originType {
			scoped = append(scoped, source)
			continue
		}
		var threadReads []integrations.Tool
		for _, tool := range source.Tools {
			if tool.ConversationScoped {
				threadReads = append(threadReads, tool)
			}
		}
		if len(threadReads) > 0 {
			source.Tools = threadReads
			scoped = append(scoped, source)
		}
	}
	return scoped
}

// bindDuplicateToolNames keeps two Integrations of one type independently reachable. A
// single Integration retains the provider's stable Tool name; only collisions gain the
// full Integration identity, so model APIs receive deterministic unique names.
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

// suppressedRun records an identical repeat that was not re-executed, with an in-band
// note the model reads in its next turn: where the original result sits, and the
// allowed next moves.
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

// callIdentityOf canonicalises one call for duplicate detection. json.Marshal renders
// map keys sorted, so two identical argument sets render identically.
func callIdentityOf(call toolCall) string {
	encoded, err := json.Marshal(call.Arguments)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", call.Arguments))
	}
	return call.Tool + " " + string(encoded)
}

// orientationTokens estimates what an orientation costs before a single read has happened.
// The tool definitions are counted too, because a catalog of forty tools is not free and a
// budget that ignored them would be a budget that overflowed on the tools alone.
func orientationTokens(oriented orientation) int {
	total := EstimateTokens(oriented.Subject) + EstimateTokens(oriented.Question)
	for _, identity := range oriented.Inventory {
		total += EstimateTokens(identity)
	}
	for _, run := range oriented.Preflight {
		total += runTokens(run)
	}
	for _, source := range oriented.Sources {
		total += EstimateTokens(source.Integration.Name)
		for _, tool := range source.Tools {
			total += EstimateTokens(tool.Name) + EstimateTokens(tool.Description) +
				EstimateTokens(tool.WhenToUse) + EstimateTokens(tool.WhenNotToUse)
		}
	}
	if oriented.Brief != nil {
		total += briefTokens(*oriented.Brief)
	}
	return total
}

// runTokens estimates what feeding one result back costs. The CONTENT is what fills a
// transcript — the summary is one line and the payload is everything the vendor returned —
// so it is what the estimate is mostly of.
func runTokens(run investigation.ToolRun) int {
	total := EstimateTokens(run.Tool) + EstimateTokens(run.Summary) +
		EstimateTokens(run.Error)
	for _, source := range run.Sources {
		total += EstimateTokens(source)
	}
	if run.Content != nil {
		if encoded, err := json.Marshal(run.Content); err == nil {
			total += EstimateTokens(string(encoded))
		}
	}
	return total
}

// concludeReason says why reads are over, written for the model to act on.
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
	case investigation.StoppedByStagnation:
		return "Your recent reads produced no new evidence."
	case investigation.StoppedByContext:
		return "This turn has filled the working context available to it."
	default:
		return ""
	}
}

// wallClockAlmostOver reports whether less than reserve remains before ctx's deadline.
// No deadline is never almost over.
func wallClockAlmostOver(ctx context.Context, reserve time.Duration) bool {
	deadline, has := ctx.Deadline()
	return has && time.Until(deadline) < reserve
}

// boundActions keeps proposed actions inside the record's bounds; the decode
// side enforces the same limits, so this is the runner's own defensive copy of them.
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

// orientation assembles what the investigator is given: only what the platform already
// holds. The trigger and the inventory are best-effort — an unreadable one narrows the
// orientation, never fails the investigation.
func (r *Agent) orientation(
	ctx context.Context, organization tenancy.Organization, opened investigation.Investigation,
	offered []offeredSource, brief *investigation.Brief,
) orientation {
	oriented := orientation{
		Subject:     opened.Subject,
		Question:    opened.Question,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Sources:     offered,
		// Prior cited findings and a bounded Message tail, or nil for a single-shot
		// Investigation that has no Conversation to continue.
		Brief: brief,
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

// selections adapts the offered sources to the executor's shape.
func selections(offered []offeredSource) []selection {
	selections := make([]selection, 0, len(offered))
	for _, source := range offered {
		selections = append(selections, selection{
			integration: source.Integration, tools: source.Tools,
		})
	}
	return selections
}

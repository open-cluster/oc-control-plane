package investigation

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
)

// THE AUTONOMOUS LOOP — one autonomous investigator behind the Runner shell. The
// shell guarantees concurrency and drain, provenance recorded as it happens, one
// audited credential unseal per integration, the citation check, and the detached
// write window for the final record. The loop drives one held Exchange over the
// whole offered tool universe, under safety ceilings — every one an honest stopped-by
// conclusion, never a silent cut and never resource exhaustion dressed up as a
// diagnosis.

// The autonomous loop's own bounds. Runs and turns are evaluation-derived tuning: they
// start as defaults and are configuration, not constants — the Runner's fields
// override them. The rest are resource protection.
const (
	// defaultMaxToolRuns bounds the whole investigation's reads.
	defaultMaxToolRuns = 30
	// defaultMaxTurns bounds the Exchange's reading turns; the turn after the last
	// is the forced conclusion.
	defaultMaxTurns = 20
	// maxStagnantTurns is how many consecutive turns may produce no new evidence before
	// the conclusion is forced. A turn with a fresh successful read resets the count.
	maxStagnantTurns = 2
	// wallClockReserve is how much of the investigation's deadline is kept back for the
	// concluding turn: an Exchange that would run into the deadline concludes with
	// what it has instead of dying mid-read as a failure.
	wallClockReserve = 2 * time.Minute
	// inventoryDigestLimit bounds the orientation's workload digest.
	inventoryDigestLimit = 50
)

// errProvenance marks a run that could not be recorded. It aborts the investigation
// wherever it surfaces — a read whose record failed must not inform a conclusion.
var errProvenance = errors.New("a tool run could not be recorded")

// errNoConclusion marks an Exchange that would not conclude when its reads were
// withdrawn.
var errNoConclusion = errors.New("the reasoner did not conclude when required to")

// run is one whole investigation: orientation from held context, then an Exchange
// of moves and executions, then the conclusion or the failure.
func (r *Runner) run(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) {
	events := newStream(r.Events, r.Telemetry, organization, opened.ID)
	startedAt := time.Now()

	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		r.fail(ctx, organization, opened.ID, events,
			"the connected sources could not be read", Spend{})
		return
	}
	brief := r.conversationBrief(ctx, organization, opened, events)
	offered := offeredSourcesForConversation(r.Catalog, candidates, brief)
	if opened.ConversationID != uuid.Nil && brief == nil {
		var safe []OfferedSource
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
	r.announce(ctx, events, EventStarted, startedPayload(opened, len(offered), true))
	for rank, source := range offered {
		recorded := Source{
			IntegrationID: source.Integration.ID,
			Rank:          rank + 1,
			Reason:        "offered to the autonomous investigator",
			SelectedAt:    time.Now().UTC(),
		}
		if err := r.Store.RecordSource(ctx, organization, opened.ID, recorded); err != nil {
			r.fail(ctx, organization, opened.ID, events,
				"the offer could not be recorded", Spend{})
			return
		}
	}

	oriented := r.orientation(ctx, organization, opened, offered, brief)
	loop := &autonomousLoop{
		runner:       r,
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
		budget:             r.ContextBudget,
		ceiling:            r.ContextCeiling,
		executedIdentities: map[string]int{},
	}
	if loop.maxRuns <= 0 {
		loop.maxRuns = defaultMaxToolRuns
	}
	if loop.maxTurns <= 0 {
		loop.maxTurns = defaultMaxTurns
	}
	for _, call := range preflightCalls(oriented) {
		result, _, preflightErr := loop.executeCall(ctx, call)
		if preflightErr != nil {
			r.fail(ctx, organization, opened.ID, events,
				"preflight provenance could not be recorded", loop.spend)
			return
		}
		oriented.Preflight = append(oriented.Preflight, result.Run)
	}
	exchange, err := r.Investigator.OpenExchange(ctx, oriented)
	if err != nil {
		r.fail(ctx, organization, opened.ID, events, reasonerFailure(err), loop.spend)
		return
	}

	// The orientation is what the transcript opens with, so it is what the turn's own
	// context starts at. Everything read afterwards adds to it.
	loop.carried = orientationTokens(oriented)

	conclusion, stoppedBy, err := loop.converse(ctx, exchange)
	if err != nil {
		r.fail(ctx, organization, opened.ID, events, failureReason(err), loop.spend)
		r.Telemetry.ended(time.Since(startedAt), StatusFailed.String(), "")
		return
	}
	r.conclude(ctx, organization, opened.ID, events, conclusion,
		len(loop.runs), loop.turns, loop.executed, stoppedBy, loop.spend)
	r.Telemetry.ended(time.Since(startedAt), StatusConcluded.String(), stoppedBy)
}

func preflightCalls(oriented Orientation) []AgentCall {
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

	var runtime, events []AgentCall
	for _, source := range oriented.Sources {
		if source.Integration.Type != integrations.TypeKubernetes {
			continue
		}
		for _, tool := range source.Tools {
			base := strings.SplitN(tool.Name, "__", 2)[0]
			switch {
			case base == "kubernetes.workload.runtime" && exactWorkload:
				runtime = append(runtime, AgentCall{
					ID: "preflight-runtime-" + source.Integration.ID.String(), Tool: tool.Name,
					Purpose: "establish the exact workload's current runtime state",
					Arguments: map[string]any{"namespace": namespace,
						"workloadKind": workloadKind, "workloadName": workloadName},
				})
			case base == "kubernetes.namespace.events" && exactNamespace:
				events = append(events, AgentCall{
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

// autonomousLoop is one investigation's execution state: the ordinal space, the
// duplicate map, the read budget and the spend.
type autonomousLoop struct {
	runner       *Runner
	organization tenancy.Organization
	opened       Investigation
	offered      []OfferedSource
	brief        *Brief
	events       *stream
	credentials  *credentialCache
	maxRuns      int
	maxTurns     int
	// budget is how many tokens of transcript this turn may accumulate before the
	// concluding turn is forced. Zero means no budget.
	budget int
	// ceiling is the total this turn may carry before its conclusion is forced. It sits
	// above budget so a bounded Conversation orientation still leaves room for Tools and
	// the concluding answer.
	ceiling int
	// carried is the running estimate of what this turn's transcript costs: the
	// orientation it opened with, plus every result fed back since.
	carried int

	runs               []ToolRun
	executedIdentities map[string]int
	executed           int
	turns              int
	spend              Spend
}

// converse drives the Exchange to its conclusion: moves in, executions out, until
// the model concludes or a ceiling forces it to.
func (l *autonomousLoop) converse(
	ctx context.Context, exchange Exchange,
) (Conclusion, string, error) {
	var results []CallResult
	stagnant := 0
	stoppedBy := ""

	for turn := 1; ; turn++ {
		// Cancellation is checked HERE rather than left to the model boundary to notice.
		// The run is cancelled when this worker's lease is lost, and whether it actually
		// stops must not depend on a vendor adapter honouring a context — a worker that
		// kept reading for an investigation another worker now owns is the exact thing
		// the fence exists to prevent.
		if err := ctx.Err(); err != nil {
			return Conclusion{}, "", err
		}
		l.turns++
		if stoppedBy == "" {
			if stoppedBy = l.firedCeiling(ctx, turn, stagnant); stoppedBy != "" {
				l.runner.announce(ctx, l.events, EventProgress,
					progressPayload(ceilingProgress(stoppedBy)))
			}
		}
		mustConclude := stoppedBy != "" || len(l.offered) == 0

		move, moveErr := l.nextMove(ctx, exchange, results, mustConclude,
			concludeReason(stoppedBy, len(l.offered)))
		l.spend = l.spend.Add(move.Spend)
		if moveErr != nil {
			return Conclusion{}, "", moveErr
		}

		if move.Conclusion != nil {
			return *move.Conclusion, stoppedBy, nil
		}
		if mustConclude {
			return Conclusion{}, "", errNoConclusion
		}

		// A fresh slice per turn: the Exchange may hold what it was fed.
		results = make([]CallResult, 0, len(move.Calls))
		freshRead := false
		for _, call := range move.Calls {
			result, fresh, err := l.executeCall(ctx, call)
			if err != nil {
				return Conclusion{}, "", err
			}
			if fresh {
				freshRead = true
			}
			l.carried += runTokens(result.Run)
			results = append(results, result)
		}
		if freshRead {
			stagnant = 0
		} else {
			stagnant++
		}
	}
}

// executeCall performs one proposed call and records it, whatever became of it: an
// identical repeat is suppressed with the original named, a call past the read budget
// is dropped visibly, and everything else executes as a read. The second return says
// whether the call produced fresh evidence — what the stagnation guard counts.
func (l *autonomousLoop) executeCall(
	ctx context.Context, call AgentCall,
) (CallResult, bool, error) {
	if call.Tool == UpdateHypothesesToolName {
		snapshot, err := decodeHypothesisSnapshot(call.Arguments, len(l.runs))
		result := CallResult{CallID: call.ID, Semantic: true, Run: ToolRun{
			Tool: UpdateHypothesesToolName, Outcome: RunSucceeded,
			Summary: "hypothesis snapshot accepted",
		}}
		if err != nil {
			result.Run.Outcome = RunFailed
			result.Run.Error = err.Error()
			// The validation failure is a semantic tool result for the model, not a loop failure.
			return result, false, nil //nolint:nilerr
		}
		result.Run.Content = map[string]any{"accepted": true}
		l.runner.announce(ctx, l.events, EventHypothesesUpdated,
			hypothesesUpdatedPayload(snapshot))
		return result, false, nil
	}
	if strings.TrimSpace(call.Purpose) == "" {
		now := time.Now().UTC()
		run := ToolRun{
			Ordinal: l.nextOrdinal(), Tool: call.Tool, Arguments: call.Arguments,
			HypothesisID: bounded(call.HypothesisID, eventTextBound),
			WindowFrom:   l.opened.WindowFrom, WindowUntil: l.opened.WindowUntil,
			Outcome: RunFailed, Error: "not executed: an external read requires a purpose",
			StartedAt: now, FinishedAt: now,
		}
		if err := l.record(ctx, run); err != nil {
			return CallResult{}, false, err
		}
		return CallResult{CallID: call.ID, Run: run}, false, nil
	}

	identity := callIdentityOf(call)
	fresh := false
	executedRead := false
	var run ToolRun

	switch {
	case l.executedIdentities[identity] != 0:
		run = suppressedRun(l.opened, call, l.nextOrdinal(), l.executedIdentities[identity])
		// A suppressed repeat is not a read, so it is not a tool event. It is still worth
		// saying: somebody watching should see that the agent asked for something it
		// already had, rather than a silent pause.
		l.runner.announce(ctx, l.events, EventProgress, progressPayload(
			"Skipped a repeat of "+l.offeredName(call.Tool)+
				"; the earlier read already answers it"))
	case l.executed >= l.maxRuns:
		run = droppedRun(l.opened, ToolCall{Tool: call.Tool, Arguments: call.Arguments},
			l.nextOrdinal(), fmt.Sprintf(
				"not executed: the investigation's read budget of %d was exhausted",
				l.maxRuns))
		l.runner.announce(ctx, l.events, EventProgress, progressPayload(
			"Did not run "+l.offeredName(call.Tool)+"; the read budget is exhausted"))
	default:
		ordinal := l.nextOrdinal()
		l.announceToolStarted(ctx, call, ordinal)
		run = l.runner.execute(ctx, l.opened, selections(l.offered), l.credentials, l.brief,
			ToolCall{Tool: call.Tool, Arguments: call.Arguments}, ordinal)
		l.runner.announce(ctx, l.events, EventToolCompleted, toolCompletedPayload(run))
		l.runner.Telemetry.ranTool(run)
		l.executed++
		// An honest empty answer is still a fresh read — "nothing changed in the
		// window" is information, and a loop that punished it as stagnation would rush
		// exactly the investigations that should rule things out.
		fresh = run.Outcome == RunSucceeded
		executedRead = true
	}
	run.Purpose = bounded(call.Purpose, eventTextBound)
	run.HypothesisID = bounded(call.HypothesisID, eventTextBound)

	if err := l.record(ctx, run); err != nil {
		return CallResult{}, false, err
	}
	if executedRead {
		l.executedIdentities[identity] = run.Ordinal
	}
	return CallResult{CallID: call.ID, Run: run}, fresh, nil
}

func decodeHypothesisSnapshot(arguments map[string]any, runs int) ([]HypothesisResult, error) {
	document, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encoding the hypothesis snapshot: %w", err)
	}
	var input struct {
		Hypotheses []struct {
			ID        string           `json:"id"`
			Statement string           `json:"statement"`
			Status    HypothesisStatus `json:"status"`
			Test      string           `json:"test"`
			RunRefs   []int            `json:"run_refs"`
		} `json:"hypotheses"`
	}
	if err := json.Unmarshal(document, &input); err != nil {
		return nil, fmt.Errorf("the hypothesis snapshot is not the declared document: %w", err)
	}
	if len(input.Hypotheses) > MaxHypothesisSnapshotItems {
		return nil, fmt.Errorf("the hypothesis snapshot has %d items; the limit is %d",
			len(input.Hypotheses), MaxHypothesisSnapshotItems)
	}
	seen := make(map[string]bool, len(input.Hypotheses))
	result := make([]HypothesisResult, 0, len(input.Hypotheses))
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
		result = append(result, HypothesisResult{
			ID:        bounded(hypothesis.ID, eventTextBound),
			Statement: bounded(hypothesis.Statement, eventTextBound), Status: hypothesis.Status,
			Test: bounded(hypothesis.Test, eventTextBound), RunRefs: hypothesis.RunRefs,
		})
	}
	return result, nil
}

func hypothesisStatusAllowed(status HypothesisStatus) bool {
	for _, allowed := range HypothesisStatuses {
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
func (l *autonomousLoop) offeredName(tool string) string {
	if _, _, offered := toolNamed(selections(l.offered), tool); offered {
		return tool
	}
	return "a tool that is not offered"
}

// announceToolStarted says which read is about to happen and where it is going, resolving
// the integration from the offered sources with the same lookup the execution itself uses,
// so the event names the source the read actually reaches.
func (l *autonomousLoop) announceToolStarted(
	ctx context.Context, call AgentCall, ordinal int,
) {
	integration, name := "", ""
	if source, _, offered := toolNamed(selections(l.offered), call.Tool); offered {
		integration = source.integration.ID.String()
		name = source.integration.Name
	}
	payload := toolStartedPayload(ToolRun{
		Ordinal: ordinal, Tool: call.Tool, Purpose: call.Purpose,
		HypothesisID: call.HypothesisID, Arguments: call.Arguments,
	}, integration)
	if name != "" {
		payload["integration"] = name
	}
	l.runner.announce(ctx, l.events, EventToolStarted, payload)
}

// record writes one run into the provenance, in ordinal order.
func (l *autonomousLoop) record(ctx context.Context, run ToolRun) error {
	l.runs = append(l.runs, run)
	if err := l.runner.Store.RecordToolRun(
		ctx, l.organization, l.opened.ID, run); err != nil {
		return errProvenance
	}
	return nil
}

// nextOrdinal is the ordinal the next recorded run takes.
func (l *autonomousLoop) nextOrdinal() int { return len(l.runs) + 1 }

// firedCeiling names the ceiling that ends the reads now, empty when none has. Checked
// in cost order; which one fired is recorded, and the model is told why its reads are
// over — the reason is part of the prompt.
func (l *autonomousLoop) firedCeiling(ctx context.Context, turn, stagnant int) string {
	switch {
	case l.runner.SpendCeilingMicroCents > 0 &&
		l.spend.MicroCents >= l.runner.SpendCeilingMicroCents:
		return StoppedBySpend
	case l.executed >= l.maxRuns:
		return StoppedByToolRuns
	case turn > l.maxTurns:
		return StoppedByReasonerTurns
	case wallClockAlmostOver(ctx, wallClockReserve):
		return StoppedByWallClock
	case stagnant >= maxStagnantTurns:
		return StoppedByStagnation
	// The transcript alone would outgrow the model's working budget. Within one turn
	// there is no surgery: per-result caps already bound each read, and if the whole
	// still crosses the line the conclusion is FORCED rather than the turn failing or
	// being silently cut. One mechanism, at one level, is the whole design.
	case l.ceiling > 0 && l.carried >= l.ceiling:
		return StoppedByContext
	default:
		return ""
	}
}

// nextMove asks the Exchange for its next move under the per-turn deadline.
func (l *autonomousLoop) nextMove(
	ctx context.Context, exchange Exchange, results []CallResult,
	mustConclude bool, reason string,
) (Move, error) {
	moveCtx, done := context.WithTimeout(ctx, decideTimeout)
	defer done()
	return exchange.Next(moveCtx, results, mustConclude, reason)
}

// failureReason renders an Exchange error as the recordable failure reason: the
// loop's own sentinels speak for themselves, anything else came from the model boundary.
func failureReason(err error) string {
	if errors.Is(err, errProvenance) || errors.Is(err, errNoConclusion) {
		return err.Error()
	}
	return reasonerFailure(err)
}

// conclude checks the conclusion and writes it, with the ceiling that forced it —
// empty when the model concluded freely. The scale of the Exchange — turns taken,
// reads executed — is context-size instrumentation, emitted here because the loop is
// the only place that knows both numbers.
func (r *Runner) conclude(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	events *stream, conclusion Conclusion, runs, turns, executed int, stoppedBy string,
	spend Spend,
) {
	if citation := checkCitations(conclusion.Findings, runs); citation != "" {
		r.fail(ctx, organization, id, events, citation, spend)
		return
	}
	// Bounded ONCE, here, before anything reads it. The record, the streamed checkpoint and
	// the terminal event all carry the answer, and bounding them separately is how a reader
	// watching the stream sees a cut nobody marked while the stored answer says it was cut.
	conclusion.Summary = boundedSummary(conclusion.Summary)
	conclusion.Actions = boundActions(conclusion.Actions)

	writeCtx, done := writeWindow(ctx)
	defer done()
	if err := r.Store.ConcludeInvestigation(writeCtx, organization, id,
		conclusion, stoppedBy, spend); err != nil {
		r.Logger.Error("an investigation's conclusion could not be recorded",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
		return
	}
	// The answer as one checkpoint, then the terminal event. Today's providers deliver the
	// concluding document whole, so there is one delta and it is final; the shape is the
	// streaming one, so a provider that later delivers it in pieces changes nothing a
	// reader has to learn.
	if conclusion.Summary != "" {
		r.announce(writeCtx, events, EventAnswerDelta,
			answerDeltaPayload(conclusion.Summary, true))
	}
	r.announce(writeCtx, events, EventConcluded, concludedPayload(conclusion, stoppedBy))
	r.Logger.Info("investigation concluded",
		slog.String("investigation_id", id.String()),
		slog.Int("turns", turns),
		slog.Int("tool_runs", executed),
		slog.String("stopped_by", stoppedBy),
		slog.Int64("spend_microcents", spend.MicroCents))
}

// offeredSources is every enabled candidate whose verified grants support at least one
// tool, in stable name order: the investigator's whole universe, derived from verified
// reality.
func offeredSources(
	catalog integrations.Catalog, candidates []integrations.Integration,
) []OfferedSource {
	var sources []OfferedSource
	for _, candidate := range candidates {
		definition, known := catalog.ByID(candidate.Type)
		if !known {
			continue
		}
		tools := offeredTools(definition, candidate)
		if len(tools) == 0 {
			continue
		}
		sources = append(sources, OfferedSource{Integration: candidate, Tools: tools})
	}
	bindDuplicateToolNames(sources)
	sortSourcesByName(sources)
	return sources
}

// offeredSourcesForConversation confines a provider-originated Conversation to its
// originating thread. Other provider categories remain available, while another
// installation of the originating provider and its broader reads are not implied.
func offeredSourcesForConversation(
	catalog integrations.Catalog, candidates []integrations.Integration, brief *Brief,
) []OfferedSource {
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

	var scoped []OfferedSource
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
func bindDuplicateToolNames(sources []OfferedSource) {
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
func suppressedRun(opened Investigation, call AgentCall, ordinal, original int) ToolRun {
	now := time.Now().UTC()
	return ToolRun{
		Ordinal:     ordinal,
		Tool:        call.Tool,
		Arguments:   call.Arguments,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Outcome:     RunFailed,
		Error: fmt.Sprintf("not executed: identical to run %d, whose result is already "+
			"above; call a different tool, or the same tool with different arguments, "+
			"to gather new evidence — or conclude", original),
		StartedAt:  now,
		FinishedAt: now,
	}
}

// callIdentityOf canonicalises one call for duplicate detection. json.Marshal renders
// map keys sorted, so two identical argument sets render identically.
func callIdentityOf(call AgentCall) string {
	encoded, err := json.Marshal(call.Arguments)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", call.Arguments))
	}
	return call.Tool + " " + string(encoded)
}

// orientationTokens estimates what an orientation costs before a single read has happened.
// The tool definitions are counted too, because a catalog of forty tools is not free and a
// budget that ignored them would be a budget that overflowed on the tools alone.
func orientationTokens(oriented Orientation) int {
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
func runTokens(run ToolRun) int {
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
	case StoppedBySpend:
		return "The investigation's spend ceiling was reached."
	case StoppedByToolRuns:
		return "The investigation's read budget was exhausted."
	case StoppedByReasonerTurns:
		return "The investigation's turn budget was exhausted."
	case StoppedByWallClock:
		return "The investigation's time is nearly over."
	case StoppedByStagnation:
		return "Your recent reads produced no new evidence."
	case StoppedByContext:
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
func boundActions(actions []ActionProposal) []ActionProposal {
	if len(actions) > MaxConclusionActions {
		actions = actions[:MaxConclusionActions]
	}
	kept := make([]ActionProposal, 0, len(actions))
	for _, action := range actions {
		action.Title = bounded(action.Title, MaxActionTextLength)
		action.Rationale = bounded(action.Rationale, MaxActionTextLength)
		action.Verification = bounded(action.Verification, MaxActionTextLength)
		kept = append(kept, action)
	}
	return kept
}

// orientation assembles what the investigator is given: only what the platform already
// holds. The trigger and the inventory are best-effort — an unreadable one narrows the
// orientation, never fails the investigation.
func (r *Runner) orientation(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
	offered []OfferedSource, brief *Brief,
) Orientation {
	oriented := Orientation{
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
func selections(offered []OfferedSource) []selection {
	selections := make([]selection, 0, len(offered))
	for _, source := range offered {
		selections = append(selections, selection{
			integration: source.Integration, tools: source.Tools,
		})
	}
	return selections
}

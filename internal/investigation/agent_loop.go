package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
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
	events := newStream(r.Events, organization, opened.ID)

	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		r.fail(ctx, organization, opened.ID, events,
			"the connected sources could not be read", Spend{})
		return
	}
	offered := offeredSources(r.Catalog, candidates)
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

	exchange, err := r.Investigator.OpenExchange(ctx,
		r.orientation(ctx, organization, opened, offered))
	if err != nil {
		r.fail(ctx, organization, opened.ID, events, reasonerFailure(err), Spend{})
		return
	}

	loop := &autonomousLoop{
		runner:       r,
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
	if loop.maxRuns <= 0 {
		loop.maxRuns = defaultMaxToolRuns
	}
	if loop.maxTurns <= 0 {
		loop.maxTurns = defaultMaxTurns
	}

	conclusion, stoppedBy, err := loop.converse(ctx, exchange)
	if err != nil {
		r.fail(ctx, organization, opened.ID, events, failureReason(err), loop.spend)
		return
	}
	r.conclude(ctx, organization, opened.ID, events, conclusion,
		len(loop.runs), loop.turns, loop.executed, stoppedBy, loop.spend)
}

// autonomousLoop is one investigation's execution state: the ordinal space, the
// duplicate map, the read budget and the spend.
type autonomousLoop struct {
	runner       *Runner
	organization tenancy.Organization
	opened       Investigation
	offered      []OfferedSource
	events       *stream
	credentials  *credentialCache
	maxRuns      int
	maxTurns     int

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
			run, fresh, err := l.executeCall(ctx, call)
			if err != nil {
				return Conclusion{}, "", err
			}
			if fresh {
				freshRead = true
			}
			results = append(results, CallResult{CallID: call.ID, Run: run})
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
) (ToolRun, bool, error) {
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
			"Skipped a repeat of "+call.Tool+"; the earlier read already answers it"))
	case l.executed >= l.maxRuns:
		run = droppedRun(l.opened, ToolCall{Tool: call.Tool, Arguments: call.Arguments},
			l.nextOrdinal(), fmt.Sprintf(
				"not executed: the investigation's read budget of %d was exhausted",
				l.maxRuns))
		l.runner.announce(ctx, l.events, EventProgress, progressPayload(
			"Did not run "+call.Tool+"; the read budget is exhausted"))
	default:
		ordinal := l.nextOrdinal()
		l.announceToolStarted(ctx, call, ordinal)
		run = l.runner.execute(ctx, l.opened, selections(l.offered), l.credentials,
			ToolCall{Tool: call.Tool, Arguments: call.Arguments}, ordinal)
		l.runner.announce(ctx, l.events, EventToolCompleted, toolCompletedPayload(run))
		l.executed++
		// An honest empty answer is still a fresh read — "nothing changed in the
		// window" is information, and a loop that punished it as stagnation would rush
		// exactly the investigations that should rule things out.
		fresh = run.Outcome == RunSucceeded
		executedRead = true
	}

	if err := l.record(ctx, run); err != nil {
		return ToolRun{}, false, err
	}
	if executedRead {
		l.executedIdentities[identity] = run.Ordinal
	}
	return run, fresh, nil
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
		Ordinal: ordinal, Tool: call.Tool, Arguments: call.Arguments,
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
	writeCtx, done := writeWindow(ctx)
	defer done()
	if err := r.Store.ConcludeInvestigation(writeCtx, organization, id,
		Conclusion{
			Answer:    bounded(conclusion.Answer, MaxAnswerLength),
			Findings:  conclusion.Findings,
			NextSteps: boundNextSteps(conclusion.NextSteps),
		}, stoppedBy, spend); err != nil {
		r.Logger.Error("an investigation's conclusion could not be recorded",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
		return
	}
	// The answer as one checkpoint, then the terminal event. Today's providers deliver the
	// concluding document whole, so there is one delta and it is final; the shape is the
	// streaming one, so a provider that later delivers it in pieces changes nothing a
	// reader has to learn.
	if conclusion.Answer != "" {
		r.announce(writeCtx, events, EventAnswerDelta,
			answerDeltaPayload(conclusion.Answer, true))
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
		if !known || candidate.Disabled() {
			continue
		}
		tools := offeredTools(definition, candidate)
		if len(tools) == 0 {
			continue
		}
		sources = append(sources, OfferedSource{Integration: candidate, Tools: tools})
	}
	sortSourcesByName(sources)
	return sources
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

// boundNextSteps keeps the recommended actions inside the record's bounds; the decode
// side enforces the same limits, so this is the runner's own defensive copy of them.
func boundNextSteps(steps []string) []string {
	if len(steps) > MaxConclusionNextSteps {
		steps = steps[:MaxConclusionNextSteps]
	}
	kept := make([]string, 0, len(steps))
	for _, step := range steps {
		kept = append(kept, bounded(step, MaxNextStepLength))
	}
	return kept
}

// orientation assembles what the investigator is given: only what the platform already
// holds. The trigger and the inventory are best-effort — an unreadable one narrows the
// orientation, never fails the investigation.
func (r *Runner) orientation(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
	offered []OfferedSource,
) Orientation {
	oriented := Orientation{
		Subject:     opened.Subject,
		Question:    opened.Question,
		WindowFrom:  opened.WindowFrom,
		WindowUntil: opened.WindowUntil,
		Sources:     offered,
	}
	if opened.EpisodeID != uuid.Nil {
		if trigger, err := r.Store.TriggerEpisode(ctx, organization, opened.EpisodeID); err == nil {
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

package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE AUTONOMOUS LOOP — one conversational investigator, running BESIDE the
// deterministic loop behind the same Runner shell. The shell's guarantees are shared:
// concurrency and drain, provenance recorded as it happens, one audited credential
// unseal per integration, the citation check, and the detached write window for the
// final record. What differs is the drive: one held conversation instead of
// re-rendered briefs, the whole offered tool universe instead of a routed subset, and
// safety ceilings — every one an honest stopped-by conclusion, never a silent cut and
// never resource exhaustion dressed up as a diagnosis.

// The autonomous loop's own bounds. Runs and turns are evaluation-derived tuning: they
// start as defaults and are configuration, not constants — the Runner's fields
// override them. The rest are resource protection.
const (
	// defaultMaxToolRuns bounds the whole investigation's reads.
	defaultMaxToolRuns = 30
	// defaultMaxTurns bounds the conversation's reading turns; the turn after the last
	// is the forced conclusion.
	defaultMaxTurns = 20
	// maxStagnantTurns is how many consecutive turns may produce no new evidence before
	// the conclusion is forced. A turn with a fresh successful read resets the count.
	maxStagnantTurns = 2
	// wallClockReserve is how much of the investigation's deadline is kept back for the
	// concluding turn: a conversation that would run into the deadline concludes with
	// what it has instead of dying mid-read as a failure.
	wallClockReserve = 2 * time.Minute
	// inventoryDigestLimit bounds the orientation's workload digest.
	inventoryDigestLimit = 50
)

// runAutonomous is one whole autonomous investigation: orientation from held context,
// then a conversation of moves and executions, then the conclusion or the failure.
func (r *Runner) runAutonomous(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) {
	spend := Spend{}

	candidates, err := r.Store.InvestigationCandidates(ctx, organization)
	if err != nil {
		r.fail(ctx, organization, opened.ID, "the connected sources could not be read", spend)
		return
	}
	offered := offeredSources(r.Catalog, candidates)
	for rank, source := range offered {
		recorded := Source{
			IntegrationID: source.Integration.ID,
			Rank:          rank + 1,
			Reason:        "offered to the autonomous investigator",
			SelectedAt:    time.Now().UTC(),
		}
		if err := r.Store.RecordSource(ctx, organization, opened.ID, recorded); err != nil {
			r.fail(ctx, organization, opened.ID, "the offer could not be recorded", spend)
			return
		}
	}

	conversation, err := r.Investigator.OpenConversation(
		ctx, r.orientation(ctx, organization, opened, offered))
	if err != nil {
		r.fail(ctx, organization, opened.ID, reasonerFailure(err), spend)
		return
	}

	credentials := newCredentialCache(r.Sealer, func(ctx context.Context, id uuid.UUID) error {
		return r.Store.RecordCredentialUnseal(ctx, organization, id,
			"investigation "+opened.ID.String())
	})
	maxRuns := r.MaxToolRuns
	if maxRuns <= 0 {
		maxRuns = defaultMaxToolRuns
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	var runs []ToolRun
	var results []CallResult
	executedIdentities := map[string]int{}
	executed := 0
	stagnant := 0
	stoppedBy := ""

	for turn := 1; ; turn++ {
		if stoppedBy == "" {
			stoppedBy = r.firedCeiling(ctx, spend, executed, maxRuns, turn, maxTurns,
				stagnant)
		}
		mustConclude := stoppedBy != "" || len(offered) == 0

		move, moveErr := r.nextMove(ctx, conversation, results, mustConclude,
			concludeReason(stoppedBy, len(offered)))
		spend = spend.Add(move.Spend)
		if moveErr != nil {
			r.fail(ctx, organization, opened.ID, reasonerFailure(moveErr), spend)
			return
		}

		if move.Conclusion != nil {
			r.concludeAutonomous(ctx, organization, opened.ID, *move.Conclusion,
				len(runs), turn, executed, stoppedBy, spend)
			return
		}
		if mustConclude {
			r.fail(ctx, organization, opened.ID,
				"the reasoner did not conclude when required to", spend)
			return
		}

		// Execute the move's calls in order, recording every one as it happens: an
		// identical repeat is suppressed with the original named, and a call past the
		// read budget is dropped visibly — the model reads both in its next turn.
		results = results[:0]
		freshRead := false
		for _, call := range move.Calls {
			ordinal := len(runs) + 1
			var run ToolRun
			identity := callIdentityOf(call)
			switch {
			case executedIdentities[identity] != 0:
				run = suppressedRun(opened, call, ordinal, executedIdentities[identity])
			case executed >= maxRuns:
				run = droppedRun(opened, ToolCall{Tool: call.Tool, Arguments: call.Arguments},
					ordinal, fmt.Sprintf(
						"not executed: the investigation's read budget of %d was exhausted",
						maxRuns))
			default:
				run = r.execute(ctx, opened, briefSelections(offered), credentials,
					ToolCall{Tool: call.Tool, Arguments: call.Arguments}, ordinal)
				executed++
				executedIdentities[identity] = ordinal
				// An honest empty answer is still a fresh read — "nothing changed in
				// the window" is information, and a loop that punished it as
				// stagnation would rush exactly the investigations that should rule
				// things out.
				if run.Outcome == RunSucceeded {
					freshRead = true
				}
			}
			runs = append(runs, run)
			if err := r.Store.RecordToolRun(ctx, organization, opened.ID, run); err != nil {
				r.fail(ctx, organization, opened.ID, "a tool run could not be recorded", spend)
				return
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

// firedCeiling names the ceiling that ends the reads now, empty when none has. Checked
// in cost order; which one fired is recorded, and the model is told why its reads are
// over — the reason is part of the prompt.
func (r *Runner) firedCeiling(
	ctx context.Context, spend Spend, executed, maxRuns, turn, maxTurns, stagnant int,
) string {
	switch {
	case r.SpendCeilingMicroCents > 0 && spend.MicroCents >= r.SpendCeilingMicroCents:
		return StoppedBySpend
	case executed >= maxRuns:
		return StoppedByToolRuns
	case turn > maxTurns:
		return StoppedByReasonerTurns
	case wallClockAlmostOver(ctx, wallClockReserve):
		return StoppedByWallClock
	case stagnant >= maxStagnantTurns:
		return StoppedByStagnation
	default:
		return ""
	}
}

// nextMove asks the conversation for its next move under the per-turn deadline.
func (r *Runner) nextMove(
	ctx context.Context, conversation Conversation, results []CallResult,
	mustConclude bool, reason string,
) (Move, error) {
	moveCtx, done := context.WithTimeout(ctx, decideTimeout)
	defer done()
	return conversation.Next(moveCtx, results, mustConclude, reason)
}

// concludeAutonomous checks the conclusion and writes it, with the ceiling that forced
// it — empty when the model concluded freely. The scale of the conversation — turns
// taken, reads executed — is context-size instrumentation, emitted here because the
// loop is the only place that knows both numbers.
func (r *Runner) concludeAutonomous(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
	conclusion Conclusion, runs, turns, executed int, stoppedBy string, spend Spend,
) {
	if citation := checkCitations(conclusion.Findings, runs); citation != "" {
		r.fail(ctx, organization, id, citation, spend)
		return
	}
	writeCtx, done := writeWindow(ctx)
	defer done()
	if err := r.Store.ConcludeInvestigation(writeCtx, organization, id,
		conclusion.Findings, boundNextSteps(conclusion.NextSteps), stoppedBy,
		spend); err != nil {
		r.Logger.Error("an investigation's conclusion could not be recorded",
			slog.String("investigation_id", id.String()),
			slog.String("error", err.Error()))
		return
	}
	r.Logger.Info("autonomous investigation concluded",
		slog.String("investigation_id", id.String()),
		slog.Int("turns", turns),
		slog.Int("tool_runs", executed),
		slog.String("stopped_by", stoppedBy),
		slog.Int64("spend_microcents", spend.MicroCents))
}

// offeredSources is every enabled candidate whose verified grants support at least one
// tool, in stable name order: the autonomous investigator's whole universe. The grant
// filter is the router's own, so availability derives from verified reality on both
// loops identically.
func offeredSources(
	catalog integrations.Catalog, candidates []integrations.Integration,
) []BriefSource {
	var sources []BriefSource
	for _, candidate := range candidates {
		definition, known := catalog.ByID(candidate.Type)
		if !known || candidate.Disabled() {
			continue
		}
		tools := offeredTools(definition, candidate)
		if len(tools) == 0 {
			continue
		}
		sources = append(sources, BriefSource{Integration: candidate, Tools: tools})
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
	offered []BriefSource,
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

// briefSelections adapts the offered sources to the executor's selection shape, so the
// autonomous loop shares the deterministic loop's execute path unchanged.
func briefSelections(offered []BriefSource) []selection {
	selections := make([]selection, 0, len(offered))
	for _, source := range offered {
		selections = append(selections, selection{
			integration: source.Integration, tools: source.Tools,
		})
	}
	return selections
}

// sortSourcesByName keeps the offer order stable run to run.
func sortSourcesByName(sources []BriefSource) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Integration.Name < sources[j].Integration.Name
	})
}

package investigation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/capability"
)

// Ending a round: the output schema, the abstention path, the failure path, and the state the
// round carried while it ran.
//
// A confident conclusion without sufficient support is not a permitted outcome, so the schema
// check below runs BEFORE storage rather than after review. An abstention is a first-class result
// and carries content; a failure is the PLATFORM failing and never "nothing was found".

// conclude asks for an outcome, checks it against the output schema, and records it.
//
// The schema check runs BEFORE storage. A model response containing an uncited claim is rejected
// and does not reach the database; it is retried once, and then the round abstains — because a
// reasoner that cannot produce a citable answer twice has not produced one.
func (r Runner) conclude(ctx context.Context, execution *round) (RoundOutcome, error) {
	if err := r.Store.AdvanceLifecycle(
		ctx, execution.organization, execution.fence, LifecycleReasoning); err != nil {
		return 0, err
	}

	// Coverage is recomputed here rather than left as the brief assembled it. A capability the
	// opening plan did not use and an adaptive pass then read would otherwise still be reported as
	// unavailable, which is a coverage section describing the moment before the investigation
	// rather than the investigation.
	if err := r.Store.RecordCoverage(
		ctx, execution.organization, execution.fence, r.coverage(execution)); err != nil {
		return 0, err
	}

	// The pass one past the last adaptive one. A hypothesis proposed here has had no read
	// dispatched against it and cannot have: this is the last call the round makes.
	concludingPass := execution.controls.MaxAdaptivePasses + 1

	var lastRefusal error
	for attempt := range 2 {
		// Recomputed per attempt rather than hoisted, so a second attempt is shown what the first
		// one added. A retry answering against a stale list would place its ordinals against a
		// shorter one than admission checks them against.
		concluded, err := execution.reasoner.Conclude(
			ctx, r.deliberation(execution, execution.controls.MaxAdaptivePasses))
		if err != nil {
			return r.modelFailure(ctx, execution, err)
		}
		execution.spend(concluded.Usage)
		// Recorded before the settlings, because this same answer may settle a hypothesis it just
		// proposed — which is what lets a cause the evidence revealed be stated at all.
		if err = r.propose(ctx, execution, concluded.Hypotheses, concludingPass); err != nil {
			return 0, err
		}
		if err = r.settle(
			ctx, execution, concluded.Weighings, concluded.Settlings); err != nil {
			return 0, err
		}

		admitted, admitErr := AdmitOutcome(concluded.Draft, execution.shown())
		if admitErr != nil {
			lastRefusal = admitErr
			r.Logger.WarnContext(ctx, "a model response was refused by the output schema",
				slog.String("investigation_id", execution.held.Investigation.ID.String()),
				slog.Int("attempt", attempt+1),
				slog.String("reason", admitErr.Error()))
			continue
		}

		outcome := admitted.Outcome
		if admitted.Untested {
			// The caveat and the reason for it are written together. A demoted outcome whose gap
			// was not recorded would tell a reader an explanation is qualified without saying by
			// what, which is the shape a coverage gap exists to prevent.
			gap, gapErr := r.recordGap(ctx, execution, UntestedExplanationGap())
			if gapErr != nil {
				return 0, gapErr
			}
			outcome.RelevantGaps = append(outcome.RelevantGaps, gap.ID)
		}
		if err = r.Store.RecordOutcome(
			ctx, execution.organization, execution.fence, outcome); err != nil {
			return 0, err
		}
		if outcome.Kind == OutcomeAbstained {
			return RoundAbstained, nil
		}
		return RoundConcluded, nil
	}

	return r.abstain(ctx, execution, whyRefused(lastRefusal))
}

// whyRefused says which standard the reasoner missed, in a sentence an operator can act on.
//
// "It could not cite its claims" and "it stated a cause nothing tested" call for different things —
// the first is a decoding or prompt problem, the second says the falsification machinery did not
// carry the round — so an abstention that collapsed them would send whoever reads the case to the
// wrong place.
func whyRefused(refusal error) string {
	if errors.Is(refusal, ErrUntraced) {
		return "the reasoner stated an explanation that corresponds to no hypothesis it proposed " +
			"and the evidence supported: " + refusal.Error()
	}
	return "the reasoner could not produce an answer whose claims cite the evidence they rest " +
		"on: " + refusal.Error()
}

// propose records hypotheses an answer produced that nothing earlier had, stamping the pass that
// proposed them so a reader can tell one the brief suggested from one the evidence forced.
func (r Runner) propose(
	ctx context.Context, execution *round, proposed []Hypothesis, pass int,
) error {
	if len(proposed) == 0 {
		return nil
	}
	stamped := make([]Hypothesis, 0, len(proposed))
	for _, hypothesis := range proposed {
		hypothesis.Pass = pass
		stamped = append(stamped, hypothesis)
	}
	recorded, err := r.Store.RecordHypotheses(
		ctx, execution.organization, execution.fence, stamped)
	if err != nil {
		return err
	}
	execution.hypotheses = append(execution.hypotheses, recorded...)
	return nil
}

// abstain records a first-class abstention carrying content: what was missing, what was left
// unresolved, and what contradicted what. An abstention with no explanation of why is a defect, so
// this one names the gaps and the live hypotheses rather than shrugging.
func (r Runner) abstain(
	ctx context.Context, execution *round, why string,
) (RoundOutcome, error) {
	draft := Draft{Kind: OutcomeAbstained, Statement: why}
	for index := range execution.gaps {
		draft.RelevantGaps = append(draft.RelevantGaps, index+1)
	}
	for index, hypothesis := range execution.hypotheses {
		if hypothesis.State == HypothesisLive {
			draft.Unresolved = append(draft.Unresolved, index+1)
		}
	}

	admitted, err := AdmitOutcome(draft, execution.shown())
	if err != nil {
		// Nothing to name: no gap, no live hypothesis, no contradiction. That is itself the thing
		// to say, and it has to be sayable or the abstention path has a hole in it.
		admitted.Outcome = Outcome{
			ID:   uuid.New(),
			Kind: OutcomeAbstained,
			Statement: why + ". No coverage gap and no unresolved hypothesis was recorded, so " +
				"this round has nothing further to name.",
		}
	}
	if err = r.Store.RecordOutcome(
		ctx, execution.organization, execution.fence, admitted.Outcome); err != nil {
		return 0, err
	}
	return RoundAbstained, nil
}

// modelFailure ends a round the model boundary could not answer.
//
// The provider being unavailable produces a FAILED round and never a conclusion. An outage has to
// produce an honest failure: a guess offered in its place is the one outcome that ends the
// product's credibility with a team.
func (r Runner) modelFailure(
	ctx context.Context, execution *round, cause error,
) (RoundOutcome, error) {
	if !errors.Is(cause, ErrModelUnavailable) &&
		!errors.Is(cause, ErrTranscriptKeyMismatch) && ctx.Err() == nil {
		return 0, cause
	}
	r.Logger.ErrorContext(ctx, "the model boundary could not answer",
		slog.String("investigation_id", execution.held.Investigation.ID.String()),
		slog.String("round_id", execution.held.Round.ID.String()),
		slog.String("error", cause.Error()))

	// The gap names WHICH failure it was. Every one of them ends the round identically, so a single
	// sentence for all of them left a reader unable to tell the vendor's problem from this build's
	// defect from an operator's limit doing its job — and the case file is the only thing a blind
	// scorer sees.
	if err := r.record(ctx, execution, Validated{Gaps: []Gap{{
		Cause:        GapCapabilityUnavailable,
		CapabilityID: "reasoning",
		Subject:      "the model provider (" + failureName(cause) + ")",
		Consequence: "the reasoning step could not run (" + failureName(cause) + "), so no " +
			"explanation was formed from what was gathered",
	}}}); err != nil && !errors.Is(err, ErrLeaseLost) {
		return 0, err
	}
	return RoundFailed, nil
}

// recordLimitReached says which limit stopped the looking and what that cost. Reaching one is
// not a failure,
// and it is not silent either: a round that stopped looking has to say why it stopped.
func (r Runner) recordLimitReached(ctx context.Context, execution *round) {
	if execution.exhausted == 0 {
		execution.exhausted = LimitRequests
	}
	if err := r.record(ctx, execution, Validated{Gaps: []Gap{{
		Cause:        GapLimitReached,
		CapabilityID: "",
		Subject:      execution.exhausted.String(),
		Consequence: "this round stopped looking when its " + execution.exhausted.String() +
			" bound ran out, so anything it had not yet asked for is not part of this case",
	}}}); err != nil && !errors.Is(err, ErrLeaseLost) {
		r.Logger.ErrorContext(ctx, "an execution limit could not be recorded",
			slog.String("investigation_id", execution.held.Investigation.ID.String()),
			slog.String("error", err.Error()))
	}
}

// coverage reports what is known about each typed capability after the opening reads.
//
// A capability the customer's stack does not provide is NOT a gap. This build only carries
// Kubernetes reads and only reaches Kubernetes Connections, so nothing is not-applicable here yet;
// the state exists because reporting a missing Nomad capability as a gap is how a coverage report
// stops being read, and the shape has to be right before the second integration arrives.
func (r Runner) coverage(execution *round) []Coverage {
	coverage := make([]Coverage, 0, len(capability.Registered()))
	for _, descriptor := range capability.Registered() {
		entry := Coverage{
			CapabilityID:      descriptor.ID,
			CapabilityVersion: descriptor.Version,
		}
		if !execution.controls.Permits(descriptor.ID) {
			entry.State = CoverageUnavailable
			entry.Reason = "not permitted by local policy"
			coverage = append(coverage, entry)
			continue
		}

		for _, item := range execution.evidence {
			if item.CapabilityID == descriptor.ID {
				entry.Evidence++
			}
		}
		switch {
		case entry.Evidence > 0:
			entry.State = CoverageChecked
			entry.Reason = "the read returned and produced evidence"
		case execution.truncated(descriptor.ID):
			entry.State = CoverageIncomplete
			entry.Reason = "the read did not finish, so nothing here supports an absence"
		case execution.attempted(descriptor.ID):
			entry.State = CoverageCheckedEmpty
			entry.Reason = "the read completed and found nothing"
		default:
			entry.State = CoverageUnavailable
			entry.Reason = "no read of this capability was made in this round"
		}
		coverage = append(coverage, entry)
	}
	return coverage
}

// deliberation is what the reasoner is shown, in the order its ordinals refer to.
func (r Runner) deliberation(execution *round, pass int) Deliberation {
	remaining := execution.controls.MaxRequests - execution.spent.Requests
	return Deliberation{
		Brief:      execution.held.Round.Brief,
		Hypotheses: execution.hypotheses,
		Evidence:   execution.evidence,
		Gaps:       execution.gaps,
		Available:  availableCapabilities(execution.controls),
		Remaining:  max(remaining, 0),
		Pass:       pass,
	}
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (e *round) spend(usage Usage) {
	e.spent.Tokens += usage.Tokens
	e.spent.MicroCents += usage.MicroCents
	if e.controls.MaxMicroCents > 0 && e.spent.MicroCents >= e.controls.MaxMicroCents {
		e.exhausted = LimitCost
	}
}

// failedBeforeLooking reports the outcome for a round that reached an execution limit before it
// produced anything.
//
// Reaching a limit is normally NOT a failure: the round concludes or abstains on what it has, and
// records what it reached as a CoverageGap. This is the one exception, and it is a platform failure
// rather than an abstention because there is nothing to abstain FROM — an abstention says the
// investigation looked and found nothing sufficient, and this run never got far enough to look.
func (e *round) failedBeforeLooking() (RoundOutcome, bool) {
	if e.exhausted != 0 && !e.produced {
		return RoundFailed, true
	}
	return 0, false
}

func (e *round) outOfLimits() bool {
	if e.exhausted != 0 {
		return true
	}
	return e.controls.MaxRequests > 0 && e.spent.Requests >= e.controls.MaxRequests
}

func (e *round) hypothesisAt(ordinal int) (Hypothesis, bool) {
	if ordinal < 1 || ordinal > len(e.hypotheses) {
		return Hypothesis{}, false
	}
	return e.hypotheses[ordinal-1], true
}

func (e *round) evidenceAt(ordinal int) (Item, bool) {
	if ordinal < 1 || ordinal > len(e.evidence) {
		return Item{}, false
	}
	return e.evidence[ordinal-1], true
}

// shown is what admission checks a draft against: everything the reasoner was given, plus which
// hypotheses this round actually put at risk.
func (e *round) shown() Shown {
	return Shown{
		Evidence:   e.evidence,
		Gaps:       e.gaps,
		Hypotheses: e.hypotheses,
		Tested:     e.tested(),
	}
}

// tested is the hypotheses at least one DISPATCHED read pointed at.
//
// It is read from what this control plane sent rather than from what the planner proposed: a
// request refused before dispatch never reached a cluster and so disproved nothing, and taking the
// proposal as evidence of a test is how a hypothesis could be "tested" by a read that was refused
// for naming a namespace outside the case's scope.
func (e *round) tested() map[uuid.UUID]struct{} {
	put := make(map[uuid.UUID]struct{}, len(e.sent))
	for _, request := range e.sent {
		if request.Justification != uuid.Nil {
			put[request.Justification] = struct{}{}
		}
	}
	return put
}

// truncated reports whether a capability's read produced a truncation gap, which is what keeps a
// bounded read from being reported as a completed one.
func (e *round) truncated(capabilityID string) bool {
	for _, gap := range e.gaps {
		if gap.CapabilityID != capabilityID {
			continue
		}
		if gap.Cause == GapResultTruncated || gap.Cause == GapAuthorizationDenied ||
			gap.Cause == GapSourceUnreachable || gap.Cause == GapRetentionHorizon {
			return true
		}
	}
	return false
}

// attempted reports whether a read of this capability was made at all.
func (e *round) attempted(capabilityID string) bool {
	for _, request := range e.sent {
		if request.CapabilityID == capabilityID {
			return true
		}
	}
	return false
}

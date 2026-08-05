package investigation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// One bounded execution, from the deterministic opening to a conclusion or an abstention.
//
// Nothing in this file logs evidence content. Identifiers, counts, outcomes and reasons do;
// the text a customer's systems produced does not, because investigating must not become a
// disclosure channel.

// pollInterval is how often a dispatched read is checked for. It is short because a round holds a
// lease while it waits and a long interval spends that lease on sleeping.
const pollInterval = 250 * time.Millisecond

// Runner executes one round of one investigation.
type Runner struct {
	Store    Store
	Reasoner Reasoner
	Logger   *slog.Logger
	// Versions is what this build pins into every round it opens, so a recorded transcript made for
	// different components is detected rather than silently replayed.
	Versions Versions
	// Transcripts records what the model said in each round and files it. Nil records nothing,
	// which is what a deployment that named nowhere to put recordings gets.
	Transcripts Transcripts
	// Now is the clock. Injected so a test can bound a round without waiting one out.
	Now func() time.Time
}

// round is the state one execution carries. It exists so the phases below read as phases rather
// than as a function threading nine values through itself.
type round struct {
	organization tenancy.Organization
	held         Claimed
	fence        Fence
	controls     Controls
	started      time.Time
	// reasoner is the boundary THIS round talks to. It is here rather than on the Runner because a
	// round being recorded gets a recorder of its own: one shared across concurrent rounds would
	// accumulate all of them into a transcript that replays as none of them.
	reasoner Reasoner
	// recording is the same value when this round is being recorded, and nil when it is not.
	recording Transcribed

	spent      Spend
	hypotheses []Hypothesis
	evidence   []Item
	gaps       []Gap
	// sent is every read this round dispatched, in the order it dispatched them.
	sent []Request
	// knownPods is what the brief resolved. A log read may name one of these and nothing else.
	knownPods []string
	// exhausted names the execution limit this round reached, when it reached one.
	exhausted Limit
	// produced reports whether this round got anything at all. A limit reached before it did is a
	// platform failure rather than an abstention.
	produced bool
}

// Run executes one claimed round to a terminal outcome.
//
// It returns an error only when the round could not be ENDED — a storage failure, or a lease lost
// to another execution. Everything else, including the model provider being unavailable and every
// execution limit being reached, is an outcome rather than an error, because those are things the
// case has to record rather than things the process should retry.
func (r Runner) Run(ctx context.Context, organization tenancy.Organization, held Claimed) error {
	controls := held.Round.Controls
	if controls.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, controls.Deadline)
		defer cancel()
	}

	execution := &round{
		organization: organization,
		held:         held,
		fence:        held.Fence(),
		controls:     controls,
		started:      r.now(),
		reasoner:     r.Reasoner,
	}
	if r.Transcripts != nil {
		execution.recording = r.Transcripts.Begin(r.Reasoner)
		execution.reasoner = execution.recording
	}

	outcome, err := r.execute(ctx, execution)
	if err != nil {
		if errors.Is(err, ErrLeaseLost) {
			// Another execution owns this round now, or it was cancelled. Stopping is the correct
			// answer: retrying would be the duplicate execution the fence exists to prevent.
			r.Logger.InfoContext(ctx, "investigation round released to another execution",
				slog.String("investigation_id", held.Investigation.ID.String()),
				slog.String("round_id", held.Round.ID.String()))
			return nil
		}
		// The round could not be carried out for a reason the case cannot record from inside
		// itself. It is failed rather than left running, because a round nothing is working on that
		// still reads as running is what a stalled case looks like.
		r.Logger.ErrorContext(ctx, "investigation round failed",
			slog.String("investigation_id", held.Investigation.ID.String()),
			slog.String("round_id", held.Round.ID.String()),
			slog.String("error", err.Error()))
		outcome = RoundFailed
	}
	// Filed before the round is finished, not after. A storage failure closing the round is
	// precisely when somebody will want to read what the model said, and filing afterwards would
	// lose the recording on the one path where it is worth most.
	r.fileTranscript(ctx, execution)

	execution.spent.Duration = r.now().Sub(execution.started)
	// Spend is recorded before the round is finished, because finishing releases the lease and a
	// fenced write after that would be refused — which would lose the figure an operator prices the
	// feature with.
	if spendErr := r.Store.RecordSpend(
		ctx, organization, execution.fence, execution.spent); spendErr != nil &&
		!errors.Is(spendErr, ErrLeaseLost) {
		return spendErr
	}

	finishErr := r.Store.FinishRound(ctx, organization, execution.fence, Finish{
		Outcome:   outcome,
		Exhausted: execution.exhausted,
	})
	if errors.Is(finishErr, ErrLeaseLost) {
		return nil
	}
	if finishErr != nil {
		return finishErr
	}

	r.Logger.InfoContext(ctx, "investigation round finished",
		slog.String("investigation_id", held.Investigation.ID.String()),
		slog.String("round_id", held.Round.ID.String()),
		slog.String("outcome", outcome.String()),
		slog.Int("evidence", len(execution.evidence)),
		slog.Int("coverage_gaps", len(execution.gaps)),
		slog.Int("requests", execution.spent.Requests))
	return nil
}

// fileTranscriptTimeout bounds writing a recording. It is short because nothing waits on it and
// the round is already over; a filing that hung would hold a worker slot for a file.
const fileTranscriptTimeout = 10 * time.Second

// fileTranscript writes what the model said in this round, when this deployment records at all.
//
// Two decisions are load-bearing. The context is detached from the round's, because a round that
// reached its deadline is one of the rounds most worth reading and filing under a cancelled
// context would record nothing exactly then. And a failure to file is logged rather than
// returned: the case is already durable, and losing an investigation because a directory filled
// up would be trading the product for its diagnostics.
func (r Runner) fileTranscript(ctx context.Context, execution *round) {
	if r.Transcripts == nil || execution.recording == nil {
		return
	}
	// Rendered against the versions the ROUND was pinned with rather than the ones this build
	// carries now. They are the same today; they stop being the same the moment a prompt changes
	// while a round is in flight, and a recording keyed on the wrong one replays against wording
	// that never produced it.
	transcript := execution.recording.Transcript(execution.held.Round.Versions)

	filing, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileTranscriptTimeout)
	defer cancel()

	if err := r.Transcripts.File(filing, execution.held, transcript); err != nil {
		r.Logger.ErrorContext(ctx, "a round's model transcript could not be filed",
			slog.String("investigation_id", execution.held.Investigation.ID.String()),
			slog.String("round_id", execution.held.Round.ID.String()),
			slog.String("error", err.Error()))
	}
}

// execute is the round itself: orient, reason, gather, reason again, and end.
func (r Runner) execute(ctx context.Context, execution *round) (RoundOutcome, error) {
	if err := r.brief(ctx, execution); err != nil {
		return 0, err
	}
	if outcome, done := execution.failedBeforeLooking(); done {
		return outcome, nil
	}

	proposed, err := execution.reasoner.Hypotheses(ctx, execution.held.Round.Brief)
	if err != nil {
		return r.modelFailure(ctx, execution, err)
	}
	execution.spend(proposed.Usage)
	// Pass zero: proposed from the brief alone, before any evidence text existed. That is what
	// makes the opening comparable between runs, and what tells a later reader which explanations
	// were predicted rather than discovered.
	if err = r.propose(ctx, execution, proposed.Hypotheses, 0); err != nil {
		return 0, err
	}

	for pass := 1; pass <= execution.controls.MaxAdaptivePasses; pass++ {
		more, passErr := r.adapt(ctx, execution, pass)
		if passErr != nil {
			if errors.Is(passErr, ErrModelUnavailable) {
				return r.modelFailure(ctx, execution, passErr)
			}
			return 0, passErr
		}
		if !more {
			break
		}
	}
	// Checked again after the passes, not only after the brief. A limit reached during an adaptive
	// pass leaves the round in exactly the same position — it stopped looking before it had
	// anything — and a check that only ran once would let that end in an abstention, which would
	// say the investigation looked and found nothing sufficient when it never got that far.
	if outcome, done := execution.failedBeforeLooking(); done {
		return outcome, nil
	}

	return r.conclude(ctx, execution)
}

// brief assembles the deterministic orientation from the opening reads and pins it.
//
// It is written before any hypothesis exists, and it is not expected to contain the answer. A round
// that terminated at the end of it has abstained rather than investigated.
func (r Runner) brief(ctx context.Context, execution *round) error {
	if err := r.Store.AdvanceLifecycle(
		ctx, execution.organization, execution.fence, LifecycleBriefing); err != nil {
		return err
	}

	scope := execution.held.Investigation.Scope
	window := execution.held.Investigation.Window
	answered, err := r.gather(ctx, execution, openingProposals(scope, window), 0)
	if err != nil {
		return err
	}

	brief := Brief{
		Trigger:     execution.held.Investigation.Trigger,
		Window:      window,
		Available:   availableCapabilities(execution.controls),
		AssembledAt: r.now(),
		Resource: ResourceIdentity{
			Kind:      scope.WorkloadKind.String(),
			Name:      scope.WorkloadName,
			Namespace: scope.Namespace,
		},
	}
	for _, answer := range answered {
		if answer.Resource != nil {
			brief.Resource = *answer.Resource
		}
		for _, change := range answer.Changes {
			brief.RecentChanges = append(brief.RecentChanges, Change{
				At:       change.At,
				Summary:  change.Summary,
				Evidence: answer.Items[change.Item].ID,
			})
		}
		for _, fact := range answer.Topology {
			brief.Topology = append(brief.Topology, TopologyFact{
				Pod:      fact.Pod,
				Node:     fact.Node,
				Owner:    fact.Owner,
				Phase:    fact.Phase,
				Ready:    fact.Ready,
				Evidence: answer.Items[fact.Item].ID,
			})
			execution.knownPods = append(execution.knownPods, fact.Pod)
		}
	}
	brief.Coverage = r.coverage(execution)

	if err = r.Store.RecordCoverage(
		ctx, execution.organization, execution.fence, brief.Coverage); err != nil {
		return err
	}
	if err = r.Store.RecordBrief(ctx, execution.organization, execution.fence, brief); err != nil {
		return err
	}
	execution.held.Round.Brief = brief
	return nil
}

// adapt runs one adaptive pass and reports whether another is worth running.
func (r Runner) adapt(ctx context.Context, execution *round, pass int) (bool, error) {
	if err := r.Store.AdvanceLifecycle(
		ctx, execution.organization, execution.fence, LifecycleReasoning); err != nil {
		return false, err
	}
	if execution.outOfLimits() {
		r.recordLimitReached(ctx, execution)
		return false, nil
	}

	proposed, err := execution.reasoner.Requests(ctx, r.deliberation(execution, pass))
	if err != nil {
		return false, err
	}
	execution.spend(proposed.Usage)
	// Recorded before the settlings and before the reads, so a hypothesis this pass discovered can
	// be settled by this same answer and can justify one of the reads it is asking for.
	if err = r.propose(ctx, execution, proposed.Hypotheses, pass); err != nil {
		return false, err
	}
	if err = r.settle(ctx, execution, proposed.Weighings, proposed.Settlings); err != nil {
		return false, err
	}
	if len(proposed.Proposals) == 0 {
		// The planner has nothing further to ask. That is a decision rather than an exhaustion, and
		// it is what a round that has what it needs looks like.
		return false, nil
	}

	if _, err = r.gather(ctx, execution, proposed.Proposals, pass); err != nil {
		return false, err
	}
	if execution.exhausted != 0 {
		r.recordLimitReached(ctx, execution)
		return false, nil
	}
	return true, nil
}

package investigation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Asking for evidence: validating what the planner proposed, dispatching what survives,
// waiting for it, and turning what comes back into EvidenceItems and CoverageGaps.
//
// Every proposal is recorded whatever becomes of it. A refused one is recorded with its reason
// and never dispatched, which is what makes the record show what was TRIED rather than only what
// worked — and what lets evidence selection be scored apart from the conclusion (ADR-009).

// gather validates, dispatches and interprets a set of proposed reads.
//
// Every proposal is recorded whatever becomes of it. A refused one is recorded with its reason and
// never dispatched, which is what makes the record show what was tried rather than only what
// worked — and what makes evidence selection scorable apart from the conclusion.
func (r Runner) gather(
	ctx context.Context, execution *round, proposals []Proposal, pass int,
) ([]Validated, error) {
	bounds := Bounds{
		Scope:      execution.held.Investigation.Scope,
		Window:     execution.held.Investigation.Window,
		Controls:   execution.controls,
		Spent:      execution.spent,
		Hypotheses: execution.hypotheses,
		KnownPods:  execution.knownPods,
		Pass:       pass,
	}

	dispatched := make([]Request, 0, len(proposals))
	for _, proposal := range proposals {
		bounds.Spent = execution.spent
		admission := Admit(proposal, bounds)

		recorded, err := r.Store.RecordRequest(
			ctx, execution.organization, execution.fence, admission.Request)
		if err != nil {
			return nil, err
		}
		if !admission.Admitted {
			r.Logger.InfoContext(ctx, "capability request refused before dispatch",
				slog.String("investigation_id", execution.held.Investigation.ID.String()),
				slog.String("capability_id", proposal.CapabilityID),
				slog.String("refusal", admission.Refusal.String()))
			if err = r.record(ctx, execution, Validated{Gaps: []Gap{{
				Cause:        GapRequestRefused,
				CapabilityID: proposal.CapabilityID,
				Subject:      proposal.CapabilityID,
				Consequence: "the read was refused before dispatch (" +
					admission.Refusal.String() + "), so what it would have shown is not here",
			}}}); err != nil {
				return nil, err
			}
			if admission.Refusal == RefusedLimitReached {
				execution.exhausted = LimitRequests
			}
			continue
		}

		job, err := r.Store.Dispatch(ctx, execution.organization, Sending{
			InvestigationID:   execution.held.Investigation.ID,
			ConnectionID:      execution.held.Investigation.Scope.Connection,
			CapabilityID:      recorded.CapabilityID,
			CapabilityVersion: recorded.CapabilityVersion,
			Arguments:         recorded.Arguments,
		})
		if err != nil {
			if errors.Is(err, ErrConnectionUnusable) {
				recorded.State = RequestFailed
				if settleErr := r.Store.SettleRequest(
					ctx, execution.organization, execution.fence, recorded); settleErr != nil {
					return nil, settleErr
				}
				continue
			}
			return nil, err
		}

		recorded.JobID = job
		recorded.State = RequestDispatched
		if err = r.Store.SettleRequest(
			ctx, execution.organization, execution.fence, recorded); err != nil {
			return nil, err
		}
		execution.spent.Requests++
		execution.sent = append(execution.sent, recorded)
		dispatched = append(dispatched, recorded)
	}

	if len(dispatched) == 0 {
		return nil, nil
	}
	return r.await(ctx, execution, dispatched)
}

// await waits for dispatched reads and interprets what comes back.
//
// A read that never settles inside the request timeout is recorded as failed and produces a gap.
// Waiting indefinitely would spend the round's whole deadline on one read, and a read nobody
// bounded is the unpriced loop every execution limit here exists to prevent.
func (r Runner) await(
	ctx context.Context, execution *round, dispatched []Request,
) ([]Validated, error) {
	if err := r.Store.AdvanceLifecycle(
		ctx, execution.organization, execution.fence, LifecycleGathering); err != nil {
		return nil, err
	}

	waiting := make(map[uuid.UUID]Request, len(dispatched))
	jobs := make([]uuid.UUID, 0, len(dispatched))
	for _, request := range dispatched {
		waiting[request.JobID] = request
		jobs = append(jobs, request.JobID)
	}

	deadline := r.now().Add(execution.controls.RequestTimeout)
	settled := make(map[uuid.UUID]Read, len(dispatched))
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for len(waiting) > 0 && r.now().Before(deadline) {
		reads, err := r.Store.Reads(ctx, execution.organization, jobs)
		if err != nil {
			return nil, err
		}
		for _, read := range reads {
			if _, still := waiting[read.JobID]; still && read.Settled {
				settled[read.JobID] = read
				delete(waiting, read.JobID)
			}
		}
		if len(waiting) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			// The round's own deadline. Whatever has not landed is recorded as not having landed.
			execution.exhausted = LimitDeadline
			return r.interpret(ctx, execution, dispatched, settled, waiting)
		case <-ticker.C:
		}
	}
	if len(waiting) > 0 {
		execution.exhausted = LimitDeadline
	}
	return r.interpret(ctx, execution, dispatched, settled, waiting)
}

// interpret turns settled reads into evidence and gaps, and records the ones that never returned.
//
// It walks the reads in DISPATCH order rather than the order they came back in. That is what makes
// evidence ordinals reproducible: two runs over the same cluster produce the same evidence in the
// same order, whichever read the cluster happened to answer first — and a reasoner cites evidence
// by ordinal, so an order decided by network timing would make a recorded transcript cite a
// different item on every replay.
func (r Runner) interpret(
	ctx context.Context, execution *round, dispatched []Request,
	settled map[uuid.UUID]Read, waiting map[uuid.UUID]Request,
) ([]Validated, error) {
	answers := make([]Validated, 0, len(settled))
	for _, request := range dispatched {
		read, landed := settled[request.JobID]
		if !landed {
			continue
		}
		validated := Interpret(request, read,
			execution.held.Investigation.Scope, execution.held.Investigation.Window,
			execution.held.Investigation.Scope.Connection)

		recorded, err := r.recordValidated(ctx, execution, validated)
		if err != nil {
			return nil, err
		}
		request.ResultBytes = int64(len(read.Result))
		request.State = RequestAnswered
		if len(recorded.Items) == 0 {
			// Kept, not discarded. What was asked, why, and that it yielded nothing is the record
			// that tells which reads earn their place.
			request.State = RequestUnproductive
		}
		if err = r.Store.SettleRequest(
			ctx, execution.organization, execution.fence, request); err != nil {
			return nil, err
		}
		execution.spent.ResultBytes += request.ResultBytes
		if len(recorded.Items) > 0 {
			execution.produced = true
		}
		answers = append(answers, recorded)
	}

	for _, request := range waiting {
		request.State = RequestFailed
		if err := r.Store.SettleRequest(
			ctx, execution.organization, execution.fence, request); err != nil {
			return nil, err
		}
		// The limit this round waits under, not a truncation: the read did not stop at a bound of
		// its own, this round stopped waiting for it. Calling it truncated would tell a reader the
		// source answered partially when it has not answered at all.
		if err := r.record(ctx, execution, Validated{Gaps: []Gap{{
			Cause:        GapLimitReached,
			CapabilityID: request.CapabilityID,
			Subject:      request.CapabilityID,
			Consequence: "the read had not returned when this round's limit on waiting was " +
				"reached, so what it would have shown is not part of this case",
		}}}); err != nil {
			return nil, err
		}
	}

	if execution.controls.MaxResultBytes > 0 &&
		execution.spent.ResultBytes >= execution.controls.MaxResultBytes {
		execution.exhausted = LimitResultBytes
	}
	return answers, nil
}

// recordValidated writes items and gaps and returns them with the identities storage assigned, so
// the brief's citations point at rows that exist.
func (r Runner) recordValidated(
	ctx context.Context, execution *round, validated Validated,
) (Validated, error) {
	recorded := validated
	if len(validated.Items) > 0 {
		items, err := r.Store.RecordEvidence(
			ctx, execution.organization, execution.fence, validated.Items)
		if err != nil {
			return Validated{}, err
		}
		recorded.Items = items
		execution.evidence = append(execution.evidence, items...)
	}
	if len(validated.Gaps) > 0 {
		gaps, err := r.Store.RecordGaps(
			ctx, execution.organization, execution.fence, validated.Gaps)
		if err != nil {
			return Validated{}, err
		}
		recorded.Gaps = gaps
		execution.gaps = append(execution.gaps, gaps...)
	}
	return recorded, nil
}

func (r Runner) record(ctx context.Context, execution *round, validated Validated) error {
	_, err := r.recordValidated(ctx, execution, validated)
	return err
}

// settle applies what the reasoner made of the evidence: how it stands towards each hypothesis, and
// which hypotheses have moved.
func (r Runner) settle(
	ctx context.Context, execution *round, weighings []Weighing, settlings []Settling,
) error {
	weighed := make([]Weighed, 0, len(weighings))
	for _, weighing := range weighings {
		hypothesis, ok := execution.hypothesisAt(weighing.Hypothesis)
		if !ok {
			continue
		}
		item, ok := execution.evidenceAt(weighing.Evidence)
		if !ok {
			continue
		}
		if weighing.Stance == 0 || weighing.Reason == "" {
			// A stance with no reason is an assertion. The point of persisting reasoning artifacts
			// is that a surprising conclusion can be examined rather than argued about.
			continue
		}
		weighed = append(weighed, Weighed{
			HypothesisID: hypothesis.ID,
			EvidenceID:   item.ID,
			Stance:       weighing.Stance,
			Reason:       weighing.Reason,
		})
	}
	if len(weighed) > 0 {
		if err := r.Store.RecordStances(
			ctx, execution.organization, execution.fence, weighed); err != nil {
			return err
		}
	}

	for _, settling := range settlings {
		hypothesis, ok := execution.hypothesisAt(settling.Hypothesis)
		if !ok || settling.State == 0 {
			continue
		}
		if settling.State == HypothesisSetAside && settling.Reason == "" {
			continue
		}
		hypothesis.State = settling.State
		hypothesis.SetAsideReason = ""
		if settling.State == HypothesisSetAside {
			hypothesis.SetAsideReason = settling.Reason
		}
		if err := r.Store.SettleHypothesis(
			ctx, execution.organization, execution.fence, hypothesis); err != nil {
			return err
		}
		execution.hypotheses[settling.Hypothesis-1] = hypothesis
	}
	return nil
}

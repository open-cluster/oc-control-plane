package relay

import (
	"errors"
	"log/slog"
	"maps"
	"time"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Sending work out: what is claimed, what is told to stop, and what cannot be sent at all.
// Nothing here decides a job's fate — the store does that before any of it reaches the wire.

// deliver claims work and sends it. It runs on the relay's hello and then periodically, so
// delivery never depends on an event notification that is not durable: a missed notification
// delays work by one interval instead of losing it.
func (s *SessionService) deliver(session *sessionState) {
	if !waitBeforeClaiming(session) {
		return
	}

	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	// Which executions this session has already asked the relay to stop. It belongs to this
	// goroutine alone, which is what makes it safe without a lock, and it is per session: a
	// successor tells the relay again, because it cannot know its predecessor's message landed.
	told := map[storage.InFlightJob]bool{}

	for {
		if err := s.deliverOnce(session, told); err != nil {
			if session.ctx.Err() != nil {
				return
			}
			session.logger.Warn("delivering work", slog.String("error", err.Error()))
		}
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// waitBeforeClaiming holds a session that arrived on the heels of another before it starts
// taking new work, and reports whether it is still worth starting at all.
//
// A session that keeps being replaced would otherwise re-run catch-up on every arrival, so a
// relay flapping — or two of them taking turns — turns into a stream of claims, leases and
// dispatches for work that goes nowhere. The wait costs nothing that matters: what the relay
// is already running was adopted when it said hello, and the ordinary case of one relay
// reconnecting after a blip waits not at all.
func waitBeforeClaiming(session *sessionState) bool {
	after := time.Duration(session.claimAfter.Load())
	if after <= 0 {
		return true
	}
	session.logger.InfoContext(session.ctx, "holding new work back from a session that keeps "+
		"being replaced", slog.Duration("for", after))

	timer := time.NewTimer(after)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-session.ctx.Done():
		return false
	}
}

func (s *SessionService) deliverOnce(
	session *sessionState, told map[storage.InFlightJob]bool,
) error {
	if err := s.dispatchWork(session); err != nil {
		return err
	}
	return s.dispatchCancellations(session, told)
}

func (s *SessionService) dispatchWork(session *sessionState) error {
	if session.draining.Load() {
		// A draining session takes no new work. Anything claimed now would be leased to a relay
		// that is disconnecting and would then sit unexecuted until its lease ran out.
		return nil
	}
	ctx := session.ctx

	// The claim commits before anything is sent. A crash between the two leaves a leased job
	// whose lease expires and is swept, which is recoverable; sending first would leave work
	// delivered but unrecorded, which is not.
	//
	// Capacity is a ceiling on what the relay holds at once, not a batch size. Claiming a
	// batch every round regardless would lease a whole backlog to one relay within minutes,
	// stranding all of it behind whatever that relay can actually run.
	claimed, err := s.database.ClaimJobs(ctx, session.organization, storage.JobClaim{
		RegistrationID: session.registrationID,
		SessionID:      session.id,
		LeaseFor:       leaseDuration,
		Capacity:       int(session.capacity.Load()),
	})
	if err != nil {
		return err
	}
	for _, job := range claimed {
		assignment, buildErr := assignmentFor(session, job)
		if buildErr != nil {
			s.failUndeliverable(session, job, buildErr)
			continue
		}
		if err = send(session, assignment); err != nil {
			return err
		}
	}
	return nil
}

// dispatchCancellations tells the relay about work it has been asked to stop, once each. A
// stop is advisory and the job stays leased until its outcome is recorded, so repeating it on
// every round would put hundreds of messages on a stream that carries real work.
func (s *SessionService) dispatchCancellations(
	session *sessionState, told map[storage.InFlightJob]bool,
) error {
	pending, err := s.database.PendingCancellations(
		session.ctx, session.organization, session.id)
	if err != nil {
		return err
	}

	// Keyed by the execution rather than the job. A lease that expires and is claimed again is
	// a different execution, and the one running now has not been told anything.
	outstanding := make(map[storage.InFlightJob]bool, len(pending))
	for _, fence := range pending {
		outstanding[storage.InFlightJob{JobID: fence.JobID, LeaseEpoch: fence.LeaseEpoch}] = true
	}
	// Forgetting what is no longer outstanding keeps this the size of the work in hand rather
	// than everything the session was ever asked to stop.
	maps.DeleteFunc(told, func(execution storage.InFlightJob, _ bool) bool {
		return !outstanding[execution]
	})

	for _, fence := range pending {
		execution := storage.InFlightJob{JobID: fence.JobID, LeaseEpoch: fence.LeaseEpoch}
		if told[execution] {
			continue
		}
		if err = send(session, cancelling(fence)); err != nil {
			return err
		}
		told[execution] = true
	}
	return nil
}

// failUndeliverable records the terminal outcome of a job that cannot be put on the wire.
//
// Two things reach here and both are ours. The arguments were encoded on this side, so a job
// that will not decode is a defect here; and a job whose typed arguments could not produce a
// usable read was planned wrong, which no execution will change either. Recording it as failed
// ends it; leaving it leased would cycle it through claim and expiry forever. Delivery then
// continues with the rest of the batch, because one undeliverable job holding up every job
// behind it turns a defect into an outage — and the jobs behind it are already leased to this
// session, so they would wait out the whole lease rather than being redelivered.
func (s *SessionService) failUndeliverable(session *sessionState, job storage.Job, cause error) {
	ctx := session.ctx

	// The same failure shape a relay would report, and the same KIND it would have reported
	// for the same cause. One taxonomy across both halves is what lets a reader of the result
	// column answer "why did this job fail" without first asking which side refused it — and a
	// capability nobody carries reported as malformed arguments would send an operator to
	// inspect a payload that was never the problem.
	outcome := storage.JobOutcome{
		Status: storage.JobFailed,
		Result: mustMarshal(&relayv1.JobFailure{Kind: refusalKind(cause)}),
	}
	fence := storage.JobFence{
		JobID: job.ID, LeaseSession: session.id, LeaseEpoch: job.LeaseEpoch,
	}

	session.logger.ErrorContext(ctx, "job refused before dispatch",
		slog.String("job_id", job.ID.String()),
		slog.String("integration_id", job.IntegrationID.String()),
		slog.String("capability_id", job.CapabilityID),
		slog.Uint64("capability_version", uint64(job.CapabilityVersion)),
		// The cause is this control plane's own message about its own job, so it carries no
		// customer data and is worth saying: without it the operator sees a job that failed
		// and no reason it could not have succeeded.
		slog.String("error", cause.Error()))
	if _, err := s.database.RecordResult(ctx, session.organization, fence, outcome); err != nil {
		session.logger.ErrorContext(ctx, "failing an undeliverable job",
			slog.String("job_id", job.ID.String()),
			slog.String("error", err.Error()))
	}
}

// refusalKind maps why a job could not be dispatched onto the wire taxonomy the relay uses for
// the same causes.
//
// The distinction it draws is the one an operator acts on. A capability or version this build
// does not carry means the two halves of the fleet disagree about what exists — someone is too
// old, and the fix is a deployment. Anything else means the job itself could not have worked,
// and the fix is in whatever planned it. Reporting the first as the second sends an operator to
// read a payload that was never wrong.
func refusalKind(cause error) relayv1.JobFailure_Kind {
	if errors.Is(cause, capability.ErrUnknownCapability) {
		return relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION
	}
	return relayv1.JobFailure_KIND_ARGUMENTS_REJECTED
}

package relay

import (
	"errors"
	"log/slog"
	"maps"
	"time"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func (s *SessionService) deliver(session *sessionState) {
	if !waitBeforeClaiming(session) {
		return
	}

	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

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
		return nil
	}
	ctx := session.ctx

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

func (s *SessionService) dispatchCancellations(
	session *sessionState, told map[storage.InFlightJob]bool,
) error {
	pending, err := s.database.PendingCancellations(
		session.ctx, session.organization, session.id)
	if err != nil {
		return err
	}

	outstanding := make(map[storage.InFlightJob]bool, len(pending))
	for _, fence := range pending {
		outstanding[storage.InFlightJob{JobID: fence.JobID, LeaseEpoch: fence.LeaseEpoch}] = true
	}
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

func (s *SessionService) failUndeliverable(session *sessionState, job storage.RelayJob, cause error) {
	ctx := session.ctx

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
		slog.String("error", cause.Error()))
	if _, err := s.database.RecordResult(ctx, session.organization, fence, outcome); err != nil {
		session.logger.ErrorContext(ctx, "failing an undeliverable job",
			slog.String("job_id", job.ID.String()),
			slog.String("error", err.Error()))
	}
}

func refusalKind(cause error) relayv1.JobFailure_Kind {
	if errors.Is(cause, capability.ErrUnknownCapability) {
		return relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION
	}
	return relayv1.JobFailure_KIND_ARGUMENTS_REJECTED
}

package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"maps"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// metadataRegistration and metadataCredential carry the relay's durable identity. Like the
// bootstrap token, they ride in metadata rather than in a message field.
const (
	metadataRegistration = "opencluster-registration-id"
	metadataCredential   = "opencluster-relay-credential"
)

// Session tuning. The lease must outlast the longest execution plus margin, or a job running
// normally could have its lease expire underneath it and be handed to someone else.
const (
	leaseDuration     = 10 * time.Minute
	heartbeatInterval = 15 * time.Second
	deliveryInterval  = 5 * time.Second
	maxJobsInFlight   = 16
	maxMessageBytes   = 1 << 20

	// How long a session may say nothing before it is treated as gone. One missed heartbeat is
	// a hiccup; three in a row is silence. The allowance is what bounds the damage a half-open
	// connection can do — until it runs out, a relay that is no longer reachable still looks
	// like the owner of every lease it holds.
	sessionIdleTimeout = 3 * heartbeatInterval

	// The budget an execution is given is deliberately shorter than the lease behind it. The
	// lease starts when the job is claimed and the relay's budget starts when it receives the
	// assignment, so a budget equal to the lease would let an execution still be running after
	// the lease authorising it has expired and the work has been handed to someone else.
	executionBudget = leaseDuration - time.Minute
)

// SessionService serves the bidirectional stream that carries work to a relay and results
// back. The stream is a delivery channel and nothing more: every guarantee about a job not
// being lost or completed twice lives in the database, so a disconnect costs a delivery
// attempt rather than a job.
type SessionService struct {
	relayv1.UnimplementedRelaySessionServiceServer

	placements *storage.Placements
	logger     *slog.Logger
	live       *liveSessions
}

// NewSessionService returns the service.
func NewSessionService(placements *storage.Placements, logger *slog.Logger) *SessionService {
	return &SessionService{
		placements: placements,
		logger:     logger,
		live:       newLiveSessions(),
	}
}

// Connect authenticates a relay, mints a session that owns every lease it takes, and runs
// until the relay disconnects, stops proving it is alive, is replaced by a reconnection, or
// the process stops.
func (s *SessionService) Connect(stream relayv1.RelaySessionService_ConnectServer) error {
	identity, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}

	session := s.open(stream.Context(), identity)
	defer s.close(session)

	// One sender goroutine owns every write. gRPC permits exactly one concurrent sender per
	// direction, and delivery and acknowledgement both produce messages, so they queue here
	// rather than racing on the stream.
	sendFailed := make(chan error, 1)
	go func() { sendFailed <- runSender(session.ctx, stream, session.outbound) }()

	if err = send(session.ctx, session.outbound, accepted(session.id.String())); err != nil {
		return session.ending(err)
	}
	session.logger.Info("relay session established")
	go watchLiveness(session)

	// Delivery deliberately does not start here. It starts on the relay's hello, so work the
	// relay is still executing is adopted before anything new is claimed — otherwise a claim
	// could take a job whose lease had expired underneath a relay that never stopped running
	// it, and that relay's finished result would have nowhere to go.

	// Receiving runs on its own goroutine so the session loop can wait on a message and on
	// every other way a session ends at the same time. A blocking receive could not notice a
	// reconnection or a silence.
	inbound := make(chan *relayv1.RelayToControl)
	readFailed := make(chan error, 1)
	go readInto(stream, inbound, readFailed)

	return s.run(session, inbound, readFailed, sendFailed)
}

// open mints the session and installs it as the live one for this registration, ending any
// predecessor. The session id is minted here, by the server, because it is the lease owner: a
// relay choosing its own could claim another's work by claiming its identity.
func (s *SessionService) open(ctx context.Context, identity relayIdentity) *sessionState {
	sessionCtx, stop := context.WithCancel(ctx)
	session := &sessionState{
		organization:   identity.organization,
		registrationID: identity.registrationID,
		id:             uuid.New(),
		outbound:       make(chan *relayv1.ControlToRelay, maxJobsInFlight),
		ctx:            sessionCtx,
		stop:           stop,
	}
	session.logger = s.logger.With(
		slog.String("organization", session.organization.String()),
		slog.String("registration_id", session.registrationID.String()),
		slog.String("session_id", session.id.String()))
	session.heard()
	session.capacity.Store(maxJobsInFlight)

	if replaced := s.live.install(session); replaced != nil {
		// A relay has one session. The one it reconnected away from would otherwise go on
		// claiming work it can no longer receive, and hold that work for a whole lease.
		replaced.logger.Info("relay session replaced by a reconnection",
			slog.String("successor_session_id", session.id.String()))
		replaced.end(codes.Aborted, "session replaced by a reconnection")
	}
	return session
}

// close ends the session and stands it down. Its leases are deliberately left to expire on
// their own clock rather than being released here: the relay may still be executing, and
// re-dispatching work that is still running is the duplicate execution the fence exists to
// prevent. A session ending costs a delivery attempt, never a job.
func (s *SessionService) close(session *sessionState) {
	session.stop()
	s.live.remove(session)
}

type relayIdentity struct {
	organization   tenancy.Organization
	registrationID uuid.UUID
}

// authenticate verifies the relay's durable credential. Every failure is one status: an
// unknown registration, a revoked one, and a wrong credential are indistinguishable, for the
// same reason enrolment refusals are.
func (s *SessionService) authenticate(ctx context.Context) (relayIdentity, error) {
	refused := status.Error(codes.Unauthenticated, "session refused")

	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return relayIdentity{}, refused
	}
	organization, err := tenancy.NewOrganization(firstValue(incoming, metadataOrganization))
	if err != nil {
		return relayIdentity{}, refused
	}
	registrationID, err := uuid.Parse(firstValue(incoming, metadataRegistration))
	if err != nil {
		return relayIdentity{}, refused
	}
	credential := firstValue(incoming, metadataCredential)
	if credential == "" {
		return relayIdentity{}, refused
	}

	digest := sha256.Sum256([]byte(credential))
	valid, err := s.placements.VerifyRelayCredential(ctx, organization, registrationID, digest[:])
	if err != nil {
		return relayIdentity{}, status.Error(codes.Unavailable, "session unavailable")
	}
	if !valid {
		s.logger.WarnContext(ctx, "relay session refused",
			slog.String("organization", organization.String()),
			slog.String("registration_id", registrationID.String()))
		return relayIdentity{}, refused
	}
	return relayIdentity{organization: organization, registrationID: registrationID}, nil
}

// run carries the session until it ends.
func (s *SessionService) run(
	session *sessionState,
	inbound <-chan *relayv1.RelayToControl,
	readFailed, sendFailed <-chan error,
) error {
	for {
		select {
		case <-session.ctx.Done():
			return session.ending(status.Error(codes.Aborted, "session ended"))

		case err := <-sendFailed:
			return session.ending(err)

		case err := <-readFailed:
			session.logger.Info("relay session ended", slog.String("reason", err.Error()))
			return session.ending(nil)

		case message := <-inbound:
			session.heard()
			if err := s.handle(session, message); err != nil {
				return session.ending(err)
			}
		}
	}
}

// handle acts on one message from the relay. Messages this version does not act on are not
// errors: the envelope evolves additively, and a relay may legitimately send more than the
// control plane has learned to use.
func (s *SessionService) handle(session *sessionState, message *relayv1.RelayToControl) error {
	if hello := message.GetHello(); hello != nil {
		s.greet(session, hello)
		return nil
	}
	if result := message.GetJobResult(); result != nil {
		return s.recordAndAcknowledge(session, result)
	}
	if acknowledged := message.GetCancelAck(); acknowledged != nil {
		// Deliberately durable-state-free. A stop that was processed still ends as a result,
		// and a job that had already finished when the stop arrived had already finished —
		// recording anything here would decide an outcome from the wrong side of the wire.
		session.logger.Info("stop acknowledged",
			slog.String("job_id", acknowledged.GetJobId()),
			slog.String("disposition", acknowledged.GetDisposition().String()))
	}
	return nil
}

// greet adopts the work a relay says it never stopped executing, then opens delivery.
//
// A relay that reconnects mid-execution is holding results it cannot record: a result must
// arrive on the session that owns the lease, and its old session is gone. Adoption moves those
// leases onto this session at the same generation, so the work survives the reconnection
// instead of being thrown away and done again.
//
// The relay's account is attested and unprivileged. What may be adopted is decided entirely by
// what the database already says, and a declaration that matches nothing changes nothing.
func (s *SessionService) greet(session *sessionState, hello *relayv1.Hello) {
	session.capacity.Store(capacityFrom(session, hello))
	session.logger.InfoContext(session.ctx, "relay said hello",
		slog.String("relay_version", hello.GetRelayVersion()),
		slog.Uint64("protocol_version", uint64(hello.GetProtocolVersion())),
		slog.Int64("capacity", session.capacity.Load()),
		slog.Int("declared_in_flight", len(hello.GetInFlight())),
		// Both are the relay's self-report of its own configuration. They are recorded here
		// for an operator to see and are never a basis for a decision on this side.
		slog.String("local_policy_hash", hello.GetLocalPolicyHash()),
		slog.Bool("endpoint_pinning_disabled", hello.GetEndpointPinningDisabled()))

	s.adopt(session, hello.GetInFlight())
	session.delivering.Do(func() { go s.deliver(session) })
}

// capacityFrom decides how much work this relay may hold at once. The relay's own figure wins
// while it is below the server's, because the relay knows what it can run and its unacked
// result buffer is sized to match; the server's is the ceiling, because a relay is not
// entitled to ask for more of the control plane than anyone else.
//
// A relay that states nothing gets the server's figure. Treating silence as a capacity of zero
// would quietly stop dispatching to it altogether, which is a worse answer than a default.
func capacityFrom(session *sessionState, hello *relayv1.Hello) int64 {
	declared := int64(hello.GetMaxConcurrentJobs())
	if declared <= 0 {
		session.logger.WarnContext(session.ctx, "relay declared no capacity; using the default",
			slog.Int("capacity", maxJobsInFlight))
		return maxJobsInFlight
	}
	return min(declared, maxJobsInFlight)
}

func (s *SessionService) adopt(session *sessionState, declared []*relayv1.InFlightJob) {
	if len(declared) == 0 {
		return
	}
	// A relay is never dispatched more than this, so a longer list describes work this control
	// plane did not assign. The excess is dropped and said aloud rather than silently trimmed.
	if len(declared) > maxJobsInFlight {
		session.logger.WarnContext(session.ctx, "relay declared more work than it can hold",
			slog.Int("declared", len(declared)),
			slog.Int("considered", maxJobsInFlight))
		declared = declared[:maxJobsInFlight]
	}

	inFlight := make([]storage.InFlightJob, 0, len(declared))
	for _, job := range declared {
		// The elapsed time a relay reports is provenance, never a fence input: a clock this
		// side does not own cannot be allowed to decide who owns a lease.
		jobID, named := namedJob(job.GetJobId())
		if !named {
			continue
		}
		inFlight = append(inFlight,
			storage.InFlightJob{JobID: jobID, LeaseEpoch: int64(job.GetLeaseEpoch())})
	}

	adopted, err := s.placements.AdoptInFlightLeases(session.ctx, session.organization,
		storage.LeaseAdoption{
			RegistrationID: session.registrationID,
			SessionID:      session.id,
			LeaseFor:       leaseDuration,
			InFlight:       inFlight,
		})
	if err != nil {
		// Delivery still opens. Nothing was adopted, so the relay's held results arrive under a
		// lease this session does not own and go unacknowledged until a later hello adopts
		// them — a delay, not a loss.
		session.logger.ErrorContext(session.ctx, "adopting in-flight work",
			slog.String("error", err.Error()))
		return
	}
	session.logger.InfoContext(session.ctx, "adopted work the relay never stopped executing",
		slog.Int("declared", len(inFlight)),
		slog.Int("adopted", len(adopted)))
}

// deliver claims work and sends it. It runs on the relay's hello and then periodically, so
// delivery never depends on an event notification that is not durable: a missed notification
// delays work by one interval instead of losing it.
func (s *SessionService) deliver(session *sessionState) {
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
	claimed, err := s.placements.ClaimJobs(ctx, session.organization, storage.JobClaim{
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
			s.failUndeliverable(session, job)
			continue
		}
		if err = send(ctx, session.outbound, assignment); err != nil {
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
	pending, err := s.placements.PendingCancellations(
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
		if err = send(session.ctx, session.outbound, cancelling(fence)); err != nil {
			return err
		}
		told[execution] = true
	}
	return nil
}

// failUndeliverable records the terminal outcome of a job that cannot be put on the wire.
//
// The arguments were encoded on this side, so a job that will not decode is a defect here and
// no execution will ever change that. Recording it as failed ends it; leaving it leased would
// cycle it through claim and expiry forever. Delivery then continues with the rest of the
// batch, because one undeliverable job holding up every job behind it turns a defect into an
// outage — and the jobs behind it are already leased to this session, so they would wait out
// the whole lease rather than being redelivered.
func (s *SessionService) failUndeliverable(session *sessionState, job storage.Job) {
	ctx := session.ctx

	// The same failure shape a relay would report, so a reader of the result column has one
	// taxonomy to understand rather than two.
	outcome := storage.JobOutcome{
		Status: storage.JobFailed,
		Result: mustMarshal(&relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_ARGUMENTS_REJECTED}),
	}
	fence := storage.JobFence{
		JobID: job.ID, LeaseSession: session.id, LeaseEpoch: job.LeaseEpoch,
	}

	session.logger.ErrorContext(ctx, "job arguments cannot be encoded for dispatch",
		slog.String("job_id", job.ID.String()),
		slog.String("capability_id", job.CapabilityID))
	if _, err := s.placements.RecordResult(ctx, session.organization, fence, outcome); err != nil {
		session.logger.ErrorContext(ctx, "failing an undeliverable job",
			slog.String("job_id", job.ID.String()),
			slog.String("error", err.Error()))
	}
}

// recordAndAcknowledge records a result and only then acknowledges it. The order is the
// guarantee: acknowledging first would let a relay stop resending a result that was never
// durably stored.
func (s *SessionService) recordAndAcknowledge(
	session *sessionState, result *relayv1.JobResult,
) error {
	ctx := session.ctx

	jobID, named := namedJob(result.GetJobId())
	if !named {
		// The message names no job, so there is nothing to record and nothing to
		// acknowledge. One malformed message must not end a session carrying other work.
		session.logger.WarnContext(ctx, "job result names no job",
			slog.String("job_id", result.GetJobId()))
		return nil
	}
	fence := storage.JobFence{
		JobID:        jobID,
		LeaseSession: session.id,
		LeaseEpoch:   int64(result.GetLeaseEpoch()),
	}

	refusal, err := s.placements.RecordResult(ctx, session.organization, fence, outcomeOf(result))
	switch {
	case err == nil:
		return send(ctx, session.outbound, resultAck(result, relayv1.ResultAck_DISPOSITION_RECORDED))

	case !errors.Is(err, storage.ErrResultRefused):
		// Nothing is acknowledged, so the relay resends. An unsent acknowledgement costs a
		// safe resend; a wrong one costs the result.
		s.logger.ErrorContext(ctx, "recording result", slog.String("error", err.Error()))
		return nil

	case refusal == storage.ResultLeaseNotHeld:
		// The execution has not been superseded — the lease is simply held elsewhere, because
		// this session never adopted it. Telling the relay to stop resending would throw away a
		// result nothing else is going to produce, so it is told nothing and keeps the result
		// until a hello adopts the lease.
		s.logger.WarnContext(ctx, "result arrived under a lease this session does not hold",
			slog.String("job_id", jobID.String()))
		return nil

	case refusal == storage.ResultAlreadyRecorded:
		return send(ctx, session.outbound,
			resultAck(result, relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED))

	default:
		// A genuinely superseded execution is told to stop resending rather than left to
		// retry: the job's fate belongs to the execution that owns it now.
		s.logger.WarnContext(ctx, "result refused",
			slog.String("job_id", jobID.String()),
			slog.String("reason", refusal.String()))
		return send(ctx, session.outbound,
			resultAck(result, relayv1.ResultAck_DISPOSITION_STALE_STOP_RESENDING))
	}
}

// namedJob reports the job a message refers to, and whether it referred to one at all.
func namedJob(identifier string) (uuid.UUID, bool) {
	parsed, err := uuid.Parse(identifier)
	return parsed, err == nil
}

// readInto moves the stream's receive side onto a channel. It ends when the stream does,
// which is guaranteed once the handler returns.
func readInto(
	stream relayv1.RelaySessionService_ConnectServer,
	inbound chan<- *relayv1.RelayToControl,
	failed chan<- error,
) {
	for {
		message, err := stream.Recv()
		if err != nil {
			failed <- err
			return
		}
		select {
		case inbound <- message:
		case <-stream.Context().Done():
			return
		}
	}
}

// runSender owns the stream's write side. Every message goes through here, so the one
// concurrent sender the transport allows is structural rather than a convention.
func runSender(
	ctx context.Context,
	stream relayv1.RelaySessionService_ConnectServer,
	outbound <-chan *relayv1.ControlToRelay,
) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case message := <-outbound:
			if err := stream.Send(message); err != nil {
				return err
			}
		}
	}
}

// send queues a message, giving up if the session is ending. The queue is bounded, so a
// relay that stops reading applies backpressure instead of growing memory here.
func send(
	ctx context.Context, outbound chan<- *relayv1.ControlToRelay, message *relayv1.ControlToRelay,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- message:
		return nil
	}
}

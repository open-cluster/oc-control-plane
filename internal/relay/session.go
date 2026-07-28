package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

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

	if err = send(session.ctx, session.outbound, accepted(session.id)); err != nil {
		return err
	}
	session.logger.Info("relay session established")

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

	if replaced := s.live.install(session); replaced != nil {
		// A relay has one session. The one it reconnected away from would otherwise go on
		// claiming work it can no longer receive, and hold that work for a whole lease.
		replaced.logger.Info("relay session replaced by a reconnection",
			slog.String("successor_session_id", session.id.String()))
		replaced.stop()
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

// sessionState is one established session: the identity every message is validated
// against, the write queue and logger scoped to it, and the switch that ends it.
type sessionState struct {
	organization   tenancy.Organization
	registrationID uuid.UUID
	id             uuid.UUID
	outbound       chan *relayv1.ControlToRelay
	logger         *slog.Logger
	ctx            context.Context
	stop           context.CancelFunc
	// delivering guards the start of delivery. A relay may say hello more than once — the
	// protocol refreshes attestations that way — and only the first one starts anything.
	delivering sync.Once
}

// liveSessions is the one live session per registration.
//
// It is per process, so it cannot see a session held by another control-plane instance. That
// is what the idle allowance is for: supersession makes the common case immediate, and the
// allowance bounds every case it cannot see.
type liveSessions struct {
	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*sessionState
}

func newLiveSessions() *liveSessions {
	return &liveSessions{byRegistration: map[uuid.UUID]*sessionState{}}
}

// install makes a session the live one and returns the session it displaced, if any.
func (l *liveSessions) install(session *sessionState) *sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	displaced := l.byRegistration[session.registrationID]
	l.byRegistration[session.registrationID] = session
	return displaced
}

// remove drops a session unless it has already been displaced: a replaced session must not
// evict its successor on the way out.
func (l *liveSessions) remove(session *sessionState) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.byRegistration[session.registrationID] == session {
		delete(l.byRegistration, session.registrationID)
	}
}

// all snapshots the live sessions, so a caller never holds the lock while working with them.
func (l *liveSessions) all() []*sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	sessions := make([]*sessionState, 0, len(l.byRegistration))
	for _, session := range l.byRegistration {
		sessions = append(sessions, session)
	}
	return sessions
}

// Drain tells every live session to stand down within the given window: stop taking new work,
// flush what it holds, and disconnect. Nothing durable changes — anything left unfinished is
// recovered by lease expiry and the sweep, which is why cutting a session short is safe.
//
// It never blocks. A relay whose queue is already full is not reading, and waiting on it would
// let one unresponsive relay decide how long a shutdown takes, which is the failure the whole
// drain path exists to bound.
func (s *SessionService) Drain(within time.Duration) {
	for _, session := range s.live.all() {
		select {
		case session.outbound <- draining(within):
		default:
			session.logger.Warn("a drain instruction could not be queued")
		}
	}
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

// deliver claims work and sends it. It runs on connect and then periodically, so delivery
// never depends on an event notification that is not durable: a missed notification delays
// work by one interval instead of losing it.
func (s *SessionService) deliver(session *sessionState) {
	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	// Which jobs this session has already been told to stop. It belongs to this goroutine
	// alone, which is what makes it safe without a lock, and it is per session: a successor
	// tells the relay again, because it has no way to know the predecessor's message landed.
	told := map[uuid.UUID]bool{}

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

func (s *SessionService) deliverOnce(session *sessionState, told map[uuid.UUID]bool) error {
	if err := s.dispatchWork(session); err != nil {
		return err
	}
	return s.dispatchCancellations(session, told)
}

// dispatchCancellations tells the relay about work it has been asked to stop, once each. A
// stop is advisory and the job stays leased until its outcome is recorded, so repeating it on
// every round would put hundreds of messages on a stream that carries real work.
func (s *SessionService) dispatchCancellations(
	session *sessionState, told map[uuid.UUID]bool,
) error {
	pending, err := s.placements.PendingCancellations(
		session.ctx, session.organization, session.id)
	if err != nil {
		return err
	}
	for _, fence := range pending {
		if told[fence.JobID] {
			continue
		}
		if err = send(session.ctx, session.outbound, cancelling(fence)); err != nil {
			return err
		}
		told[fence.JobID] = true
	}
	return nil
}

func (s *SessionService) dispatchWork(session *sessionState) error {
	ctx := session.ctx

	// The claim commits before anything is sent. A crash between the two leaves a leased job
	// whose lease expires and is swept, which is recoverable; sending first would leave work
	// delivered but unrecorded, which is not.
	claimed, err := s.placements.ClaimJobs(ctx, session.organization, storage.JobClaim{
		RegistrationID: session.registrationID,
		SessionID:      session.id,
		LeaseFor:       leaseDuration,
		Limit:          maxJobsInFlight,
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

// run carries the session until it ends. Every way a session can end is one case here, so
// none of them can be missed while waiting on another.
func (s *SessionService) run(
	session *sessionState,
	inbound <-chan *relayv1.RelayToControl,
	readFailed, sendFailed <-chan error,
) error {
	idle := time.NewTimer(sessionIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-session.ctx.Done():
			return status.Error(codes.Aborted, "session ended")

		case <-idle.C:
			// Silence is not proof of death, only the end of proof of life. The session is
			// ended and its leases are left to expire, because a relay behind a half-open
			// connection may still be executing everything it holds.
			session.logger.Warn("relay session went silent",
				slog.Duration("allowance", sessionIdleTimeout))
			return status.Error(codes.DeadlineExceeded, "session idle")

		case err := <-sendFailed:
			return err

		case err := <-readFailed:
			session.logger.Info("relay session ended", slog.String("reason", err.Error()))
			return nil

		case message := <-inbound:
			// Any message proves liveness, not only a heartbeat: a relay streaming results is
			// evidently alive. What a heartbeat adds is proof from a relay with nothing to say.
			idle.Reset(sessionIdleTimeout)
			if err := s.handle(session, message); err != nil {
				return err
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
	session.logger.InfoContext(session.ctx, "relay said hello",
		slog.String("relay_version", hello.GetRelayVersion()),
		slog.Uint64("protocol_version", uint64(hello.GetProtocolVersion())),
		slog.Int("declared_in_flight", len(hello.GetInFlight())),
		// Both are the relay's self-report of its own configuration. They are recorded here
		// for an operator to see and are never a basis for a decision on this side.
		slog.String("local_policy_hash", hello.GetLocalPolicyHash()),
		slog.Bool("endpoint_pinning_disabled", hello.GetEndpointPinningDisabled()))

	s.adopt(session, hello.GetInFlight())
	session.delivering.Do(func() { go s.deliver(session) })
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
		// Delivery still opens. Nothing was adopted, so the relay's held results will be
		// refused and the work re-executed after its leases expire — a cost, not a corruption.
		session.logger.ErrorContext(session.ctx, "adopting in-flight work",
			slog.String("error", err.Error()))
		return
	}
	session.logger.InfoContext(session.ctx, "adopted work the relay never stopped executing",
		slog.Int("declared", len(inFlight)),
		slog.Int("adopted", len(adopted)))
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
	case errors.Is(err, storage.ErrResultRefused) && refusal == storage.ResultAlreadyRecorded:
		return send(ctx, session.outbound, resultAck(result, relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED))
	case errors.Is(err, storage.ErrResultRefused):
		// A superseded lease is told to stop resending rather than left to retry: the job's
		// fate belongs to the execution that owns it now.
		s.logger.WarnContext(ctx, "result refused",
			slog.String("job_id", jobID.String()),
			slog.String("reason", refusal.String()))
		return send(ctx, session.outbound, resultAck(result, relayv1.ResultAck_DISPOSITION_STALE_STOP_RESENDING))
	default:
		// Nothing is acknowledged, so the relay resends. An unsent acknowledgement costs a
		// safe resend; a wrong one costs the result.
		s.logger.ErrorContext(ctx, "recording result", slog.String("error", err.Error()))
		return nil
	}
}

// namedJob reports the job a message refers to, and whether it referred to one at all.
func namedJob(identifier string) (uuid.UUID, bool) {
	parsed, err := uuid.Parse(identifier)
	return parsed, err == nil
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
func send(ctx context.Context, outbound chan<- *relayv1.ControlToRelay, message *relayv1.ControlToRelay) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- message:
		return nil
	}
}

func accepted(sessionID uuid.UUID) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_SessionAccepted{
		SessionAccepted: &relayv1.SessionAccepted{
			SessionId:         sessionID.String(),
			ProtocolVersion:   protocolVersion,
			HeartbeatInterval: durationpb.New(heartbeatInterval),
			MaxReceiveBytes:   maxMessageBytes,
			MaxSendBytes:      maxMessageBytes,
		},
	}}
}

// cancelling asks the relay to stop an execution. It carries the fence so the relay can tell
// which execution of the job it is being asked to stop, rather than the job in the abstract.
func cancelling(fence storage.JobFence) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_Cancellation{
		Cancellation: &relayv1.Cancellation{
			JobId:      fence.JobID.String(),
			LeaseEpoch: uint64(fence.LeaseEpoch),
		},
	}}
}

func draining(within time.Duration) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_DrainInstruction{
		DrainInstruction: &relayv1.DrainInstruction{Deadline: durationpb.New(within)},
	}}
}

func assignmentFor(session *sessionState, job storage.Job) (*relayv1.ControlToRelay, error) {
	arguments := &relayv1.CapabilityArguments{}
	if err := proto.Unmarshal(job.Arguments, arguments); err != nil {
		return nil, err
	}
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_JobAssignment{
		JobAssignment: &relayv1.JobAssignment{
			JobId: job.ID.String(),
			OrgId: session.organization.String(),
			// Both identities travel so the relay can check the assignment against the identity
			// it enrolled with; a mismatch is a possible control-plane compromise, not a routing
			// mistake to be tolerated.
			RegistrationId: session.registrationID.String(),
			CapabilityId:   job.CapabilityID,
			// The relay selects its implementation by version, so an assignment without one
			// names a capability it cannot decide how to execute.
			CapabilityVersion: job.CapabilityVersion,
			LeaseEpoch:        uint64(job.LeaseEpoch),
			DeadlineBudget:    durationpb.New(executionBudget),
			IdempotencyKey:    job.ID.String(),
			Arguments:         arguments,
		},
	}}, nil
}

func resultAck(result *relayv1.JobResult, disposition relayv1.ResultAck_Disposition) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_ResultAck{
		ResultAck: &relayv1.ResultAck{
			JobId:       result.GetJobId(),
			LeaseEpoch:  result.GetLeaseEpoch(),
			Disposition: disposition,
		},
	}}
}

// outcomeOf reduces a wire result to what is durable. A failure is recorded as a failure
// rather than discarded: an investigation that could not gather evidence is a fact about the
// investigation, not an absence of one.
func outcomeOf(result *relayv1.JobResult) storage.JobOutcome {
	if result.GetFailure() != nil {
		return storage.JobOutcome{Status: storage.JobFailed, Result: mustMarshal(result.GetFailure())}
	}
	return storage.JobOutcome{Status: storage.JobSucceeded, Result: mustMarshal(result.GetResult())}
}

// mustMarshal returns the encoded message, or nil when there is nothing to encode. A failure
// to marshal a message that was just received is not recoverable and not worth propagating;
// the outcome is still recorded, with an empty payload that is visibly wrong.
func mustMarshal(message proto.Message) []byte {
	if message == nil {
		return nil
	}
	encoded, err := proto.Marshal(message)
	if err != nil {
		return nil
	}
	return encoded
}

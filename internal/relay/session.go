package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	metadataRegistration = "opencluster-registration-id"
	metadataCredential   = "opencluster-relay-credential"
)

const (
	leaseDuration     = 10 * time.Minute
	heartbeatInterval = 15 * time.Second
	deliveryInterval  = 5 * time.Second
	maxJobsInFlight   = 16
	maxMessageBytes   = 1 << 20

	sessionIdleTimeout = 3 * heartbeatInterval

	LivenessAllowance = sessionIdleTimeout

	// Execution must expire before its authorizing lease can be reassigned.
	executionBudget = leaseDuration - time.Minute

	sendDeadline = heartbeatInterval

	flushWindow = 2 * time.Second

	recordConflictTimeout = 10 * time.Second
)

type SessionService struct {
	relayv1.UnimplementedRelaySessionServiceServer

	database          *storage.Database
	logger            *slog.Logger
	live              *liveSessions
	churn             *churnWatch
	inventoryInterval time.Duration
}

func NewSessionService(
	database *storage.Database, logger *slog.Logger, inventoryInterval time.Duration,
) *SessionService {
	return &SessionService{
		database:          database,
		logger:            logger,
		live:              newLiveSessions(),
		churn:             newChurnWatch(time.Now),
		inventoryInterval: inventoryInterval,
	}
}

func (s *SessionService) Connect(stream relayv1.RelaySessionService_ConnectServer) error {
	identity, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}

	session := s.open(stream.Context(), identity)

	sendFailed := make(chan error, 1)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		sendFailed <- runSender(session, stream)
	}()

	defer func() {
		s.close(session)
		waitForSender(senderDone)
	}()

	if err = send(session, accepted(session.id.String())); err != nil {
		return session.ending(err)
	}
	session.logger.Info("relay session established")
	s.recordPresence(session)
	go watchLiveness(session)
	go s.watchPresence(session)

	inbound := make(chan *relayv1.RelayToControl)
	readFailed := make(chan error, 1)
	go readInto(stream, inbound, readFailed)

	return s.run(session, inbound, readFailed, sendFailed)
}

func (s *SessionService) open(ctx context.Context, identity relayIdentity) *sessionState {
	sessionCtx, stop := context.WithCancel(ctx)
	session := &sessionState{
		organization:   identity.organization,
		registrationID: identity.registrationID,
		id:             uuid.New(),
		outbound:       make(chan *relayv1.ControlToRelay, maxJobsInFlight),
		ctx:            sessionCtx,
		stop:           stop,
		peer:           peerAddress(ctx),
	}
	session.logger = s.logger.With(
		slog.String("organization", session.organization.String()),
		slog.String("registration_id", session.registrationID.String()),
		slog.String("session_id", session.id.String()))
	session.heard()
	session.capacity.Store(maxJobsInFlight)

	if replaced := s.live.install(session); replaced != nil {
		s.supersede(replaced, session)
	}
	return session
}

func (s *SessionService) supersede(replaced, successor *sessionState) {
	replaced.logger.Info("relay session replaced by a reconnection",
		slog.String("successor_session_id", successor.id.String()))

	verdict := s.churn.record(successor.registrationID, successor.peer)
	successor.claimAfter.Store(int64(verdict.backoff))
	switch {
	case verdict.untracked:
		successor.logger.WarnContext(successor.ctx,
			"session takeover went uncounted; the watch is full")
	case verdict.contested:
		s.escalate(successor, verdict)
	}

	select {
	case replaced.outbound <- reconnecting(sessionIdleTimeout):
	default:
		replaced.logger.Warn("a reconnect instruction could not be queued")
	}
	replaced.end(codes.Aborted, "session replaced by a reconnection")
}

func (s *SessionService) escalate(session *sessionState, verdict churnVerdict) {
	session.logger.ErrorContext(session.ctx, "relay identity is contested",
		slog.Int("takeovers", verdict.takeovers),
		slog.Duration("within", churnWindow),
		slog.Int("distinct_hosts", verdict.distinctHosts))

	session.contested.Store(true)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), recordConflictTimeout)
	defer cancel()

	if err := s.database.RecordSessionConflict(ctx, session.organization,
		session.registrationID, verdict.distinctHosts); err != nil {
		session.logger.ErrorContext(ctx, "recording a contested relay identity",
			slog.String("error", err.Error()))
	}
}

func (s *SessionService) close(session *sessionState) {
	session.stop()
	s.live.remove(session)
	s.releasePresence(session)
}

type relayIdentity struct {
	organization   uuid.UUID
	registrationID uuid.UUID
}

func (s *SessionService) authenticate(ctx context.Context) (relayIdentity, error) {
	refused := status.Error(codes.Unauthenticated, "session refused")

	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return relayIdentity{}, refused
	}
	organization, err := uuid.Parse(strings.TrimSpace(firstValue(incoming, metadataOrganization)))
	if err != nil || organization == uuid.Nil {
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
	valid, err := s.database.VerifyRelayCredential(ctx, organization, registrationID, digest[:])
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

func (s *SessionService) handle(session *sessionState, message *relayv1.RelayToControl) error {
	switch body := message.GetMessage().(type) {
	case *relayv1.RelayToControl_Hello:
		return s.greet(session, body.Hello)

	case *relayv1.RelayToControl_JobResult:
		return s.recordAndAcknowledge(session, body.JobResult)

	case *relayv1.RelayToControl_InventoryDelta:
		return s.recordInventoryDelta(session, body.InventoryDelta)

	case *relayv1.RelayToControl_Heartbeat:
		s.recordInventoryFreshness(session, body.Heartbeat)
		note(session, message)

	default:
		note(session, message)
	}
	return nil
}

func note(session *sessionState, message *relayv1.RelayToControl) {
	switch body := message.GetMessage().(type) {
	case *relayv1.RelayToControl_Heartbeat:
		session.logger.Debug("heartbeat",
			slog.Uint64("sequence", body.Heartbeat.GetSequence()),
			slog.Uint64("in_flight", uint64(body.Heartbeat.GetInFlightCount())))

	case *relayv1.RelayToControl_JobAck:
		session.logger.Debug("assignment received",
			slog.String("job_id", body.JobAck.GetJobId()))

	case *relayv1.RelayToControl_JobStarted:
		session.logger.Debug("execution started",
			slog.String("job_id", body.JobStarted.GetJobId()))

	case *relayv1.RelayToControl_CancelAck:
		session.logger.Info("stop acknowledged",
			slog.String("job_id", body.CancelAck.GetJobId()),
			slog.String("disposition", body.CancelAck.GetDisposition().String()))

	case *relayv1.RelayToControl_DrainState:
		session.logger.Info("relay draining",
			slog.Bool("draining", body.DrainState.GetDraining()),
			slog.Uint64("in_flight", uint64(body.DrainState.GetInFlightCount())),
			slog.Uint64("unacked_results", uint64(body.DrainState.GetUnackedResultCount())))

	case *relayv1.RelayToControl_ProtocolError:
		session.logger.Error("relay refused a message from the control plane",
			slog.String("code", body.ProtocolError.GetCode().String()),
			slog.String("detail", body.ProtocolError.GetDetail()))
	}
}

func (s *SessionService) greet(session *sessionState, hello *relayv1.Hello) error {
	if !supportsProtocol(hello.GetProtocolVersion()) {
		session.logger.WarnContext(session.ctx, "relay does not speak this protocol version",
			slog.Uint64("relay_speaks_up_to", uint64(hello.GetProtocolVersion())),
			slog.Uint64("protocol_version", protocolVersion))
		return status.Error(codes.FailedPrecondition, "unsupported protocol version")
	}
	session.capacity.Store(capacityFrom(session, hello))
	session.logger.InfoContext(session.ctx, "relay said hello",
		slog.String("relay_version", hello.GetRelayVersion()),
		slog.Uint64("protocol_version", uint64(hello.GetProtocolVersion())),
		slog.Int64("capacity", session.capacity.Load()),
		slog.Int("declared_in_flight", len(hello.GetInFlight())),
		slog.String("local_policy_hash", hello.GetLocalPolicyHash()),
		slog.Bool("endpoint_pinning_disabled", hello.GetEndpointPinningDisabled()))

	if s.adopt(session, hello.GetInFlight()) && !session.contested.Load() {
		s.releaseWhatTheRelayIsNotRunning(session)
	}
	session.delivering.Do(func() { go s.deliver(session) })
	session.policies.Do(func() { go s.refreshInventoryPolicies(session) })
	return nil
}

func (s *SessionService) releaseWhatTheRelayIsNotRunning(session *sessionState) {
	released, err := s.database.ReleaseStrandedLeases(
		session.ctx, session.organization, session.registrationID, session.id)
	if err != nil {
		session.logger.ErrorContext(session.ctx, "releasing stranded work",
			slog.String("error", err.Error()))
		return
	}
	if released > 0 {
		session.logger.InfoContext(session.ctx, "released work no relay is executing",
			slog.Int64("released", released))
	}
}

func capacityFrom(session *sessionState, hello *relayv1.Hello) int64 {
	declared := int64(hello.GetMaxConcurrentJobs())
	if declared <= 0 {
		session.logger.WarnContext(session.ctx, "relay declared no capacity; using the default",
			slog.Int("capacity", maxJobsInFlight))
		return maxJobsInFlight
	}
	return min(declared, maxJobsInFlight)
}

func (s *SessionService) adopt(session *sessionState, declared []*relayv1.InFlightJob) bool {
	inFlight, complete := readRoster(session, declared)
	if len(inFlight) == 0 {
		return complete
	}

	adopted, err := s.database.AdoptInFlightLeases(session.ctx, session.organization,
		storage.LeaseAdoption{
			RegistrationID: session.registrationID,
			SessionID:      session.id,
			LeaseFor:       leaseDuration,
			InFlight:       inFlight,
		})
	if err != nil {
		session.logger.ErrorContext(session.ctx, "adopting in-flight work",
			slog.String("error", err.Error()))
		return false
	}
	session.logger.InfoContext(session.ctx, "adopted work the relay never stopped executing",
		slog.Int("declared", len(inFlight)),
		slog.Int("adopted", len(adopted)))

	return complete && len(adopted) == len(inFlight)
}

func readRoster(
	session *sessionState, declared []*relayv1.InFlightJob,
) ([]storage.InFlightJob, bool) {
	complete := true

	if len(declared) > maxJobsInFlight {
		session.logger.WarnContext(session.ctx, "relay declared more work than it can hold",
			slog.Int("declared", len(declared)),
			slog.Int("considered", maxJobsInFlight))
		declared = declared[:maxJobsInFlight]
		complete = false
	}

	inFlight := make([]storage.InFlightJob, 0, len(declared))
	for _, job := range declared {
		jobID, named := namedJob(job.GetJobId())
		if !named {
			session.logger.WarnContext(session.ctx, "relay declared work it did not name",
				slog.String("job_id", job.GetJobId()))
			complete = false
			continue
		}
		inFlight = append(inFlight,
			storage.InFlightJob{JobID: jobID, LeaseEpoch: int64(job.GetLeaseEpoch())})
	}
	return inFlight, complete
}

func (s *SessionService) recordAndAcknowledge(
	session *sessionState, result *relayv1.JobResult,
) error {
	// Record before acknowledging so a lost database write causes a safe resend, not a lost result.
	ctx := session.ctx

	jobID, named := namedJob(result.GetJobId())
	if !named {
		session.logger.WarnContext(ctx, "job result names no job",
			slog.String("job_id", result.GetJobId()))
		return nil
	}
	outcome, err := outcomeOf(result)
	if err != nil {
		session.logger.ErrorContext(ctx, "relay result cannot be read by this control plane",
			slog.String("job_id", result.GetJobId()))
		return err
	}
	fence := storage.JobFence{
		JobID:        jobID,
		LeaseSession: session.id,
		LeaseEpoch:   int64(result.GetLeaseEpoch()),
	}

	refusal, err := s.database.RecordResult(ctx, session.organization, fence, outcome)
	return s.acknowledge(session, result, refusal, err)
}

func (s *SessionService) acknowledge(
	session *sessionState,
	result *relayv1.JobResult,
	refusal storage.ResultRefusal,
	err error,
) error {
	ctx := session.ctx
	switch {
	case err == nil:
		return send(session, resultAck(result, relayv1.ResultAck_DISPOSITION_RECORDED))

	case !errors.Is(err, storage.ErrResultRefused):
		// Silence asks the Relay to resend; a false acknowledgement would discard the result.
		s.logger.ErrorContext(ctx, "recording result", slog.String("error", err.Error()))
		return nil

	case refusal == storage.ResultLeaseNotHeld:
		s.logger.WarnContext(ctx, "result arrived under a lease this session does not hold",
			slog.String("job_id", result.GetJobId()))
		return nil

	case refusal == storage.ResultAlreadyRecorded:
		return send(session,
			resultAck(result, relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED))

	default:
		s.logger.WarnContext(ctx, "result refused",
			slog.String("job_id", result.GetJobId()),
			slog.String("reason", refusal.String()))
		return send(session,
			resultAck(result, relayv1.ResultAck_DISPOSITION_STALE_STOP_RESENDING))
	}
}

func namedJob(identifier string) (uuid.UUID, bool) {
	parsed, err := uuid.Parse(identifier)
	return parsed, err == nil
}

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

func runSender(
	session *sessionState, stream relayv1.RelaySessionService_ConnectServer,
) error {
	// One goroutine owns writes because concurrent sends are forbidden by the gRPC stream contract.
	for {
		select {
		case <-session.ctx.Done():
			return flushQueued(session, stream)
		case message := <-session.outbound:
			if err := stream.Send(message); err != nil {
				return err
			}
		}
	}
}

func flushQueued(
	session *sessionState, stream relayv1.RelaySessionService_ConnectServer,
) error {
	for {
		select {
		case message := <-session.outbound:
			if err := stream.Send(message); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func waitForSender(done <-chan struct{}) {
	timer := time.NewTimer(flushWindow)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
	}
}

func send(session *sessionState, message *relayv1.ControlToRelay) error {
	timer := time.NewTimer(sendDeadline)
	defer timer.Stop()

	select {
	case <-session.ctx.Done():
		return session.ctx.Err()
	case session.outbound <- message:
		return nil
	case <-timer.C:
		session.logger.Warn("relay stream wedged; closing it",
			slog.Duration("deadline", sendDeadline))
		session.end(codes.DeadlineExceeded, "session wedged")
		return errStreamWedged
	}
}

var errStreamWedged = errors.New("relay stream wedged")

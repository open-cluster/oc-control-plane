package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
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

	// LivenessAllowance is sessionIdleTimeout under an exported name, for the read models that
	// have to decide whether a relay counts as connected.
	//
	// It is exported rather than copied because it was copied — twice, into two packages, with
	// two different justifications for why it had to be — and a fleet summary whose idea of
	// "connected" drifts from the session service's is a summary that disagrees with the thing
	// it is summarising. The dependency runs one way and there is no cycle: internal/relay is
	// imported by the surfaces, and imports none of them.
	LivenessAllowance = sessionIdleTimeout

	// The budget an execution is given is deliberately shorter than the lease behind it. The
	// lease starts when the job is claimed and the relay's budget starts when it receives the
	// assignment, so a budget equal to the lease would let an execution still be running after
	// the lease authorising it has expired and the work has been handed to someone else.
	executionBudget = leaseDuration - time.Minute

	// How long a message may wait to be queued before the stream is treated as wedged. If a
	// message cannot be handed over in the time a healthy relay takes to say it is alive, the
	// stream is not moving. A wedged stream is closed rather than left holding a writer:
	// unbounded buffering and blocked writers are the two ways a stuck relay becomes the
	// control plane's problem instead of its own.
	sendDeadline = heartbeatInterval

	// How long the sender keeps flushing what is already queued once a session ends. A parting
	// instruction is only worth sending if it can arrive, and the bound is what stops a relay
	// that has stopped reading from delaying the teardown it was told about.
	flushWindow = 2 * time.Second

	// How long recording a contested identity may take. It is detached from the session that
	// prompted it, so it needs a bound of its own or a stalled database would hold the goroutine
	// for as long as it stayed stalled.
	recordConflictTimeout = 10 * time.Second
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
	churn      *churnWatch
	// inventoryInterval is what this deployment ASKS each relay to synchronize at; the
	// relay floors it locally, so this can only slow a fleet down.
	inventoryInterval time.Duration
}

// NewSessionService returns the service.
func NewSessionService(
	placements *storage.Placements, logger *slog.Logger, inventoryInterval time.Duration,
) *SessionService {
	return &SessionService{
		placements:        placements,
		logger:            logger,
		live:              newLiveSessions(),
		churn:             newChurnWatch(time.Now),
		inventoryInterval: inventoryInterval,
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

	// One sender goroutine owns every write. gRPC permits exactly one concurrent sender per
	// direction, and delivery and acknowledgement both produce messages, so they queue here
	// rather than racing on the stream.
	sendFailed := make(chan error, 1)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		sendFailed <- runSender(session, stream)
	}()

	// The order matters and is written out rather than left to how defers stack. Standing the
	// session down is what releases the sender, and the sender must be finished before this
	// returns: once the handler returns the stream is gone, and a send still in flight would be
	// writing to it. Waiting first would wait on a sender nothing had told to stop.
	defer func() {
		s.close(session)
		waitForSender(senderDone)
	}()

	if err = send(session, accepted(session.id.String())); err != nil {
		return session.ending(err)
	}
	session.logger.Info("relay session established")
	// Presence is recorded before liveness is watched, so a summary read a moment after a relay
	// connects says it is connected rather than saying nothing until the first refresh.
	s.recordPresence(session)
	go watchLiveness(session)
	go s.watchPresence(session)

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

// supersede stands a replaced session down, telling it to reconnect on the way out, and judges
// what the takeover means.
//
// Being displaced is stated rather than merely done. A relay dropped without explanation
// re-enters its backoff and goes quiet, and the relay being displaced is not always the one
// that reconnected: something else holding its credential produces exactly this, and the
// victim's only view of it is a session that ended. It has to come back to be seen at all.
//
// The delay is a whole idle allowance, so two connections holding one identity cannot displace
// each other faster than the silence between them could ever be noticed.
func (s *SessionService) supersede(replaced, successor *sessionState) {
	replaced.logger.Info("relay session replaced by a reconnection",
		slog.String("successor_session_id", successor.id.String()))

	verdict := s.churn.record(successor.registrationID, successor.peer)
	successor.claimAfter.Store(int64(verdict.backoff))
	switch {
	case verdict.untracked:
		// Said aloud rather than passed over. A verdict that was never reached is not a verdict
		// of nothing wrong, and an operator reading the absence of alerts deserves to know the
		// difference.
		successor.logger.WarnContext(successor.ctx,
			"session takeover went uncounted; the watch is full")
	case verdict.contested:
		s.escalate(successor, verdict)
	}

	// Queued without waiting: the session is ending either way, and the sender flushes what is
	// already there on its way out.
	select {
	case replaced.outbound <- reconnecting(sessionIdleTimeout):
	default:
		replaced.logger.Warn("a reconnect instruction could not be queued")
	}
	replaced.end(codes.Aborted, "session replaced by a reconnection")
}

// escalate reports a relay identity that is being taken over faster, or by more parties, than
// reconnection explains.
//
// It is not refused. Refusing would hand anyone who can trigger this the ability to lock a
// customer's relay out of the platform, which turns a detection into a weapon. What credential
// theft actually costs is not bounded here either — anyone holding a valid credential can
// receive assignments and report whatever they like, and no amount of caution on this path
// changes that. What this does is make the pattern visible, and stop the one decision that
// would otherwise let a second party act on the first one's behalf.
func (s *SessionService) escalate(session *sessionState, verdict churnVerdict) {
	session.logger.ErrorContext(session.ctx, "relay identity is contested",
		slog.Int("takeovers", verdict.takeovers),
		slog.Duration("within", churnWindow),
		// The count of hosts, never which ones. More than one host holding a single relay's
		// credential is the whole signal, and the addresses themselves belong in connection
		// logs rather than in an alert that gets forwarded onward.
		slog.Int("distinct_hosts", verdict.distinctHosts))

	session.contested.Store(true)

	// Deliberately detached from the session. Whoever is taking the session over can also drop
	// it immediately, and a record written under the session's own context would be cancelled
	// by exactly the behaviour it exists to record.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), recordConflictTimeout)
	defer cancel()

	if err := s.placements.RecordSessionConflict(ctx, session.organization,
		session.registrationID, verdict.distinctHosts); err != nil {
		session.logger.ErrorContext(ctx, "recording a contested relay identity",
			slog.String("error", err.Error()))
	}
}

// close ends the session and stands it down. Its leases are deliberately left to expire on
// their own clock rather than being released here: the relay may still be executing, and
// re-dispatching work that is still running is the duplicate execution the fence exists to
// prevent. A session ending costs a delivery attempt, never a job.
func (s *SessionService) close(session *sessionState) {
	session.stop()
	s.live.remove(session)
	// Recorded as ended, so a clean disconnect is distinguishable from a relay that vanished.
	// Guarded on the session identifier inside storage, so a session that has already been
	// displaced cannot clear the presence its successor now holds.
	s.releasePresence(session)
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

// handle acts on one message from the relay.
//
// Three of these change durable state — the hello, a job result, and an inventory delta —
// and the heartbeat does when it carries inventory freshness stamps; the rest are the relay
// accounting for itself, and are recorded rather than acted on. They are enumerated anyway,
// because a message that arrives and is silently dropped is indistinguishable from one that
// was never sent — and one of them is the relay reporting that this side broke the protocol.
//
// An unrecognised variant is not an error. The envelope evolves additively, so a newer relay
// may legitimately say more than this control plane has learned to use.
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

// note records what the relay says about itself. None of it changes durable state: job truth
// advances only through results, and a relay's account of its own progress is not evidence of
// it. They are written down anyway, because a message that arrives and is dropped in silence
// is indistinguishable from one that was never sent.
func note(session *sessionState, message *relayv1.RelayToControl) {
	switch body := message.GetMessage().(type) {
	case *relayv1.RelayToControl_Heartbeat:
		// Liveness was already recorded when the message arrived. A heartbeat proves the
		// session is alive and nothing else: job progress advances only through the store.
		session.logger.Debug("heartbeat",
			slog.Uint64("sequence", body.Heartbeat.GetSequence()),
			slog.Uint64("in_flight", uint64(body.Heartbeat.GetInFlightCount())))

	case *relayv1.RelayToControl_JobAck:
		// Reception only. The job was durably leased before it was dispatched, so this changes
		// nothing and promises nothing about execution.
		session.logger.Debug("assignment received",
			slog.String("job_id", body.JobAck.GetJobId()))

	case *relayv1.RelayToControl_JobStarted:
		// Beginning proves nothing about finishing, and the lease already covers the job.
		session.logger.Debug("execution started",
			slog.String("job_id", body.JobStarted.GetJobId()))

	case *relayv1.RelayToControl_CancelAck:
		// Deliberately durable-state-free. A stop that was processed still ends as a result,
		// and a job that had already finished when the stop arrived had already finished —
		// recording anything here would decide an outcome from the wrong side of the wire.
		session.logger.Info("stop acknowledged",
			slog.String("job_id", body.CancelAck.GetJobId()),
			slog.String("disposition", body.CancelAck.GetDisposition().String()))

	case *relayv1.RelayToControl_DrainState:
		session.logger.Info("relay draining",
			slog.Bool("draining", body.DrainState.GetDraining()),
			slog.Uint64("in_flight", uint64(body.DrainState.GetInFlightCount())),
			slog.Uint64("unacked_results", uint64(body.DrainState.GetUnackedResultCount())))

	case *relayv1.RelayToControl_ProtocolError:
		// The relay refusing something this control plane sent. Dropping it would mean the one
		// party that can see the defect reports it to nobody, so it is the loudest thing here.
		session.logger.Error("relay refused a message from the control plane",
			slog.String("code", body.ProtocolError.GetCode().String()),
			slog.String("detail", body.ProtocolError.GetDetail()))
	}
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
func (s *SessionService) greet(session *sessionState, hello *relayv1.Hello) error {
	// The relay states the highest version it speaks, so a newer relay is negotiated down
	// rather than turned away — that is what makes the protocol able to move at all. What
	// cannot be tolerated is a relay that does not reach this version: two sides that disagree
	// about what a message means still exchange messages successfully, and what comes out is
	// evidence whose provenance nobody can vouch for. The version spoken here is in the
	// acceptance the relay already holds, so the gap is visible from both ends.
	if hello.GetProtocolVersion() < protocolVersion {
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
		// Both are the relay's self-report of its own configuration. They are recorded here
		// for an operator to see and are never a basis for a decision on this side.
		slog.String("local_policy_hash", hello.GetLocalPolicyHash()),
		slog.Bool("endpoint_pinning_disabled", hello.GetEndpointPinningDisabled()))

	// Releasing turns one relay's silence about a job into a decision that nobody is running
	// it. That is only sound while there is one relay to be silent. Under contest there are two
	// accounts of the same registration and no way to tell which is the relay's, so releasing
	// on either would let one party revoke the other's work.
	//
	// Adoption is deliberately still allowed. It is the narrower move — a lease follows the
	// connection that says it is running the job — and withholding it would strand the real
	// relay's finished work for a full lease every time this fires, including when it fires on
	// a relay whose address merely changed. What is withheld is the wider move, the one that
	// ends other executions rather than continuing one.
	if s.adopt(session, hello.GetInFlight()) && !session.contested.Load() {
		s.releaseWhatTheRelayIsNotRunning(session)
	}
	session.delivering.Do(func() { go s.deliver(session) })
	// After delivery opens, because a policy is advisory context rather than work: a relay
	// that never receives one synchronizes nothing and says so through its freshness surface,
	// while a job held back would be an investigation waiting.
	session.policies.Do(func() { go s.refreshInventoryPolicies(session) })
	return nil
}

// releaseWhatTheRelayIsNotRunning returns work the relay did not claim to be executing, so it
// can be dispatched again now instead of after a full lease.
//
// A relay has one session. Once this one has adopted everything the relay says it is still
// running, whatever is left leased to this registration belongs to a session that is gone —
// nothing is executing it and nothing is going to report on it. Waiting out the lease anyway
// would add ten minutes of nothing to the far side of every network blip.
func (s *SessionService) releaseWhatTheRelayIsNotRunning(session *sessionState) {
	released, err := s.placements.ReleaseStrandedLeases(
		session.ctx, session.organization, session.registrationID, session.id)
	if err != nil {
		// The leases stay where they are and expire on their own clock, which is the behaviour
		// this is an improvement on rather than a replacement for.
		session.logger.ErrorContext(session.ctx, "releasing stranded work",
			slog.String("error", err.Error()))
		return
	}
	if released > 0 {
		session.logger.InfoContext(session.ctx, "released work no relay is executing",
			slog.Int64("released", released))
	}
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

// adopt moves the declared leases onto this session and reports whether the relay's account
// could be taken as complete. An account that had to be trimmed is not complete, and what was
// trimmed must be left alone rather than treated as work nobody is running.
func (s *SessionService) adopt(session *sessionState, declared []*relayv1.InFlightJob) bool {
	inFlight, complete := readRoster(session, declared)
	if len(inFlight) == 0 {
		// A relay running nothing has given a complete account by saying so. That is what a
		// restarted relay looks like, and everything it once held should come back now.
		return complete
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
		// them — a delay, not a loss. Nothing may be released either: the declared work is
		// still leased where it was, and releasing it would treat work the relay is running as
		// work nobody is.
		session.logger.ErrorContext(session.ctx, "adopting in-flight work",
			slog.String("error", err.Error()))
		return false
	}
	session.logger.InfoContext(session.ctx, "adopted work the relay never stopped executing",
		slog.Int("declared", len(inFlight)),
		slog.Int("adopted", len(adopted)))

	// Anything declared but not adopted is a job the two sides see differently — already
	// finished here, or at a generation this relay no longer holds. Nothing may be released on
	// the strength of an account that did not match, because what would be released is the work
	// the disagreement is about.
	return complete && len(adopted) == len(inFlight)
}

// readRoster reduces the relay's account to what can be acted on, and reports whether the whole
// of it was understood. An account that had to be trimmed or partly skipped is not an account
// of everything the relay is running, and must not be read as one.
func readRoster(
	session *sessionState, declared []*relayv1.InFlightJob,
) ([]storage.InFlightJob, bool) {
	complete := true

	// A relay is never dispatched more than this, so a longer list describes work this control
	// plane did not assign. The excess is dropped and said aloud rather than silently trimmed —
	// what is dropped is still adopted up to the bound, because refusing the lot would strand
	// every result the relay is holding for a full lease.
	if len(declared) > maxJobsInFlight {
		session.logger.WarnContext(session.ctx, "relay declared more work than it can hold",
			slog.Int("declared", len(declared)),
			slog.Int("considered", maxJobsInFlight))
		declared = declared[:maxJobsInFlight]
		complete = false
	}

	inFlight := make([]storage.InFlightJob, 0, len(declared))
	for _, job := range declared {
		// The elapsed time a relay reports is provenance, never a fence input: a clock this
		// side does not own cannot be allowed to decide who owns a lease.
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
	// Read before anything durable happens, because a result this build cannot read must not
	// reach the store in any form — least of all as the success an unreadable outcome would
	// otherwise be mistaken for.
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

	refusal, err := s.placements.RecordResult(ctx, session.organization, fence, outcome)
	return s.acknowledge(session, result, refusal, err)
}

// acknowledge tells the relay what became of the result it sent, and says nothing at all when
// nothing definitive can be said. An unsent acknowledgement costs a safe resend; a wrong one
// costs the result.
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
			slog.String("job_id", result.GetJobId()))
		return nil

	case refusal == storage.ResultAlreadyRecorded:
		return send(session,
			resultAck(result, relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED))

	default:
		// A genuinely superseded execution is told to stop resending rather than left to
		// retry: the job's fate belongs to the execution that owns it now.
		s.logger.WarnContext(ctx, "result refused",
			slog.String("job_id", result.GetJobId()),
			slog.String("reason", refusal.String()))
		return send(session,
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
	session *sessionState, stream relayv1.RelaySessionService_ConnectServer,
) error {
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

// flushQueued sends what is already waiting when a session ends, and nothing more. A graceful
// reconnect or a drain instruction is queued at the moment the session is stood down, so
// without this the message the relay most needs is the one it never gets.
//
// There is no timer here on purpose: the loop takes only what is already in the queue and
// stops the moment it is empty. What this cannot bound is a send to a relay that has stopped
// reading, which is why the handler waits on the sender for a bounded time rather than
// indefinitely.
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

// waitForSender gives the sender a bounded chance to finish before the handler returns and the
// stream goes away underneath it.
func waitForSender(done <-chan struct{}) {
	timer := time.NewTimer(flushWindow)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
	}
}

// send queues a message for the relay.
//
// The queue is bounded, so a relay that stops reading applies backpressure rather than growing
// memory here — and the deadline is what keeps that backpressure from becoming this side's
// problem. A stream that cannot take a message within it is wedged, and a wedged stream is
// closed rather than left holding a writer indefinitely.
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

// errStreamWedged ends the delivery round that discovered the wedged stream. The session is
// already being stood down, so nothing further is attempted on it.
var errStreamWedged = errors.New("relay stream wedged")

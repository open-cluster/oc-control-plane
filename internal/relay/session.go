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
)

// SessionService serves the bidirectional stream that carries work to a relay and results
// back. The stream is a delivery channel and nothing more: every guarantee about a job not
// being lost or completed twice lives in the database, so a disconnect costs a delivery
// attempt rather than a job.
type SessionService struct {
	relayv1.UnimplementedRelaySessionServiceServer

	placements *storage.Placements
	logger     *slog.Logger
}

// NewSessionService returns the service.
func NewSessionService(placements *storage.Placements, logger *slog.Logger) *SessionService {
	return &SessionService{placements: placements, logger: logger}
}

// Connect authenticates a relay, mints a session that owns every lease it takes, and runs
// until the relay disconnects or the process stops.
func (s *SessionService) Connect(stream relayv1.RelaySessionService_ConnectServer) error {
	ctx := stream.Context()
	identity, err := s.authenticate(ctx)
	if err != nil {
		return err
	}

	// The session id is minted here, by the server, because it is the lease owner. A relay
	// choosing its own could claim another's work by claiming its identity.
	session := sessionState{
		organization:   identity.organization,
		registrationID: identity.registrationID,
		id:             uuid.New(),
		outbound:       make(chan *relayv1.ControlToRelay, maxJobsInFlight),
	}
	session.logger = s.logger.With(
		slog.String("organization", session.organization.String()),
		slog.String("registration_id", session.registrationID.String()),
		slog.String("session_id", session.id.String()))

	// One sender goroutine owns every write. gRPC permits exactly one concurrent sender per
	// direction, and delivery and acknowledgement both produce messages, so they queue here
	// rather than racing on the stream.
	sending, stopSending := context.WithCancel(ctx)
	defer stopSending()
	sendFailed := make(chan error, 1)
	go func() { sendFailed <- runSender(sending, stream, session.outbound) }()

	if err = send(ctx, session.outbound, accepted(session.id)); err != nil {
		return err
	}
	session.logger.Info("relay session established")

	go s.deliver(sending, session)

	return s.receive(stream, session, sendFailed)
}

// sessionState is one established session: the identity every message is validated
// against, plus the write queue and logger scoped to it.
type sessionState struct {
	organization   tenancy.Organization
	registrationID uuid.UUID
	id             uuid.UUID
	outbound       chan *relayv1.ControlToRelay
	logger         *slog.Logger
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
func (s *SessionService) deliver(ctx context.Context, session sessionState) {
	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()

	for {
		if err := s.deliverOnce(ctx, session); err != nil {
			if ctx.Err() != nil {
				return
			}
			session.logger.Warn("delivering work", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *SessionService) deliverOnce(ctx context.Context, session sessionState) error {
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
			// The job cannot be expressed on the wire. Leaving it leased would strand it, so
			// it is left for the lease to expire and the sweep to reconsider.
			return buildErr
		}
		if err = send(ctx, session.outbound, assignment); err != nil {
			return err
		}
	}
	return nil
}

// receive reads the relay's messages until the stream ends.
func (s *SessionService) receive(
	stream relayv1.RelaySessionService_ConnectServer,
	session sessionState,
	sendFailed <-chan error,
) error {
	for {
		select {
		case err := <-sendFailed:
			return err
		default:
		}

		message, err := stream.Recv()
		if err != nil {
			session.logger.Info("relay session ended", slog.String("reason", err.Error()))
			return nil
		}
		if result := message.GetJobResult(); result != nil {
			if err = s.recordAndAcknowledge(stream.Context(), session, result); err != nil {
				return err
			}
		}
	}
}

// recordAndAcknowledge records a result and only then acknowledges it. The order is the
// guarantee: acknowledging first would let a relay stop resending a result that was never
// durably stored.
func (s *SessionService) recordAndAcknowledge(
	ctx context.Context,
	session sessionState,
	result *relayv1.JobResult,
) error {
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

func assignmentFor(session sessionState, job storage.Job) (*relayv1.ControlToRelay, error) {
	arguments := &relayv1.CapabilityArguments{}
	if err := proto.Unmarshal(job.Arguments, arguments); err != nil {
		return nil, err
	}
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_JobAssignment{
		JobAssignment: &relayv1.JobAssignment{
			JobId:          job.ID.String(),
			OrgId:          session.organization.String(),
			RegistrationId: session.registrationID.String(),
			CapabilityId:   job.CapabilityID,
			LeaseEpoch:     uint64(job.LeaseEpoch),
			IdempotencyKey: job.ID.String(),
			Arguments:      arguments,
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

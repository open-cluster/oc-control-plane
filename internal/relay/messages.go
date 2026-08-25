package relay

import (
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Everything the control plane puts on the wire is built here, so what a relay sees is
// described in one place rather than assembled wherever it happens to be sent.

func accepted(sessionID string) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_SessionAccepted{
		SessionAccepted: &relayv1.SessionAccepted{
			SessionId:         sessionID,
			ProtocolVersion:   protocolVersion,
			HeartbeatInterval: durationpb.New(heartbeatInterval),
			MaxReceiveBytes:   maxMessageBytes,
			MaxSendBytes:      maxMessageBytes,
		},
	}}
}

// assignmentFor builds one job's wire assignment, refusing to send anything this build cannot
// vouch for.
//
// Validating here rather than only at the Relay is what makes "neither side trusts the other"
// true in both directions. The Relay re-validates on receipt and refuses what it cannot serve;
// this refuses the same thing before it costs a dispatch, a lease and a round trip — and it
// means a Relay is never the only thing standing between a planner's mistake and a customer's
// cluster.
func assignmentFor(session *sessionState, job storage.Job) (*relayv1.ControlToRelay, error) {
	if err := capability.Validate(job.CapabilityID, job.CapabilityVersion, job.Arguments); err != nil {
		return nil, err
	}
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

// reconnecting tells a relay to come back, and when. A stated reconnect is not the same as a
// connection that simply ended: an unexplained ending re-enters the relay's backoff, while
// this tells it the control plane expects it back.
func reconnecting(after time.Duration) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_GracefulReconnect{
		GracefulReconnect: &relayv1.GracefulReconnect{RetryAfter: durationpb.New(after)},
	}}
}

func draining(within time.Duration) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_DrainInstruction{
		DrainInstruction: &relayv1.DrainInstruction{Deadline: durationpb.New(within)},
	}}
}

// inventoryPolicy asks a Relay to watch one Integration's scope. It is a request by
// design: the relay floors the interval and intersects the namespaces with its own
// local configuration, so nothing sent here can increase load on a customer's cluster.
// No namespaces are named — the relay watches what its operator allows.
func inventoryPolicy(scope changeledger.Scope) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_InventorySynchronizationPolicy{
		InventorySynchronizationPolicy: &relayv1.InventorySynchronizationPolicy{
			ConnectionId:      scope.IntegrationID.String(),
			Revision:          uint64(scope.PolicyRevision),
			RequestedInterval: durationpb.New(scope.RequestedInterval),
		},
	}}
}

// inventoryDeltaAck is the relay's licence to forget one delta. Sent only after the
// recording transaction committed, or after a refusal nothing the relay does can cure.
func inventoryDeltaAck(deltaID string) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_InventoryDeltaAck{
		InventoryDeltaAck: &relayv1.InventoryDeltaAck{DeltaId: deltaID},
	}}
}

func resultAck(
	result *relayv1.JobResult, disposition relayv1.ResultAck_Disposition,
) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_ResultAck{
		ResultAck: &relayv1.ResultAck{
			JobId:       result.GetJobId(),
			LeaseEpoch:  result.GetLeaseEpoch(),
			Disposition: disposition,
		},
	}}
}

// outcomeOf reduces a wire result to what is durable, or refuses to.
//
// A failure is recorded as a failure rather than discarded: an investigation that could not
// gather evidence is a fact about the investigation, not an absence of one.
//
// Anything this build cannot read is refused instead, and the two ways that happens are the
// reason this returns an error at all. An outcome that is neither of the two known variants —
// a newer relay reporting something this version has never heard of — reads as no outcome, and
// treating it as a success with an empty payload would record that an investigation gathered
// evidence when what actually happened is that nobody here understood the answer. A payload
// carrying fields this build cannot see is the same fabrication one level down: part of what
// came back would never be looked at, and nothing downstream could know there was a hole in it.
//
// Only the capability payload is inspected. The envelope around it evolves additively by
// design, so a newer relay saying more at that level is expected and must not be refused.
func outcomeOf(result *relayv1.JobResult) (storage.JobOutcome, error) {
	unreadable := status.Error(codes.InvalidArgument, "capability payload not understood")

	switch outcome := result.GetOutcome().(type) {
	case *relayv1.JobResult_Failure:
		if carriesUnreadableFields(outcome.Failure) {
			return storage.JobOutcome{}, unreadable
		}
		jobStatus := storage.JobFailed
		if outcome.Failure.GetKind() == relayv1.JobFailure_KIND_CANCELLED {
			jobStatus = storage.JobCancelled
		}
		return storage.JobOutcome{
			Status: jobStatus, Result: mustMarshal(outcome.Failure),
		}, nil

	case *relayv1.JobResult_Result:
		if carriesUnreadableFields(outcome.Result) {
			return storage.JobOutcome{}, unreadable
		}
		return storage.JobOutcome{
			Status: storage.JobSucceeded, Result: mustMarshal(outcome.Result),
		}, nil

	default:
		return storage.JobOutcome{}, unreadable
	}
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

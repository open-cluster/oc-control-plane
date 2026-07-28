package relay

import (
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

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

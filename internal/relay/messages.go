package relay

import (
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/changes"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

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

func assignmentFor(session *sessionState, job storage.RelayJob) (*relayv1.ControlToRelay, error) {
	if err := capability.Validate(job.CapabilityID, job.CapabilityVersion, job.Arguments); err != nil {
		return nil, err
	}
	arguments := &relayv1.CapabilityArguments{}
	if err := proto.Unmarshal(job.Arguments, arguments); err != nil {
		return nil, err
	}
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_JobAssignment{
		JobAssignment: &relayv1.JobAssignment{
			JobId:             job.ID.String(),
			OrgId:             session.organization.String(),
			RegistrationId:    session.registrationID.String(),
			CapabilityId:      job.CapabilityID,
			CapabilityVersion: job.CapabilityVersion,
			LeaseEpoch:        uint64(job.LeaseEpoch),
			DeadlineBudget:    durationpb.New(executionBudget),
			IdempotencyKey:    job.ID.String(),
			Arguments:         arguments,
		},
	}}, nil
}

func cancelling(fence storage.JobFence) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_Cancellation{
		Cancellation: &relayv1.Cancellation{
			JobId:      fence.JobID.String(),
			LeaseEpoch: uint64(fence.LeaseEpoch),
		},
	}}
}

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

func inventoryPolicy(scope changes.Scope) *relayv1.ControlToRelay {
	return &relayv1.ControlToRelay{Message: &relayv1.ControlToRelay_InventorySynchronizationPolicy{
		InventorySynchronizationPolicy: &relayv1.InventorySynchronizationPolicy{
			ConnectionId:      scope.IntegrationID.String(),
			Revision:          uint64(scope.PolicyRevision),
			RequestedInterval: durationpb.New(scope.RequestedInterval),
		},
	}}
}

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

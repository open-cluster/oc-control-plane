package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const (
	capabilityUnderTest        = "kubernetes.workload.runtime"
	capabilityVersionUnderTest = 1
	protocolVersionUnderTest   = 1
)

type relayCredentials struct {
	registration uuid.UUID
	credential   string
}

func registerRelay(
	t *testing.T, connection *grpc.ClientConn, dsn, organization string, capabilities ...string,
) relayCredentials {
	t.Helper()
	if len(capabilities) == 0 {
		capabilities = []string{capabilityUnderTest}
	}
	descriptors := make([]*relayv1.CapabilityDescriptor, 0, len(capabilities))
	for _, advertised := range capabilities {
		descriptors = append(descriptors, &relayv1.CapabilityDescriptor{
			CapabilityId: advertised, CapabilityVersion: capabilityVersionUnderTest,
		})
	}

	token := "bootstrap-token-for-" + uuid.NewString()
	issueBootstrapToken(t, dsn, organization, token)

	client := relayv1.NewRelayRegistrationServiceClient(connection)
	response, err := register(t, client, organization, token, &relayv1.RegisterRequest{
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0-test",
		ClusterFingerprint: "kube-system-uid-under-test",
		Capabilities:       descriptors,
	})
	if err != nil {
		t.Fatalf("registering the relay the session needs: %v", err)
	}
	registration, err := uuid.Parse(response.GetRegistrationId())
	if err != nil {
		t.Fatalf("the issued registration identity is not an identity: %v", err)
	}
	return relayCredentials{registration: registration, credential: response.GetCredential()}
}

func connectSession(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()
	return connectSessionDeclaring(t, connection, organization, relay, nil)
}

func connectSessionDeclaring(
	t *testing.T,
	connection *grpc.ClientConn,
	organization string,
	relay relayCredentials,
	inFlight []*relayv1.InFlightJob,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()

	stream := openStream(t, connection, organization, relay)
	sayHello(t, stream, protocolVersionUnderTest, inFlight)
	return stream
}

func openStream(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	stream, err := relayv1.NewRelaySessionServiceClient(connection).
		Connect(sessionMetadata(ctx, organization, relay))
	if err != nil {
		t.Fatalf("opening the session: %v", err)
	}
	return stream
}

func sayHello(
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	protocolVersion uint32,
	inFlight []*relayv1.InFlightJob,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_Hello{
		Hello: &relayv1.Hello{
			ProtocolVersion:   protocolVersion,
			RelayVersion:      "0.1.0-test",
			MaxConcurrentJobs: 4,
			Capabilities: []*relayv1.CapabilityDescriptor{
				{CapabilityId: capabilityUnderTest, CapabilityVersion: capabilityVersionUnderTest},
			},
			InFlight: inFlight,
		},
	}})
	if err != nil {
		t.Fatalf("saying hello: %v", err)
	}
}

func refuseSession(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := relayv1.NewRelaySessionServiceClient(connection).
		Connect(sessionMetadata(ctx, organization, relay))
	if err != nil {
		t.Fatalf("opening the session: %v", err)
	}
	if _, err = stream.Recv(); err == nil {
		t.Fatal("the session was accepted; an unproven identity must be refused")
	}
	reported, ok := status.FromError(err)
	if !ok || reported.Code() != codes.Unauthenticated {
		t.Fatalf("refused with %v, want Unauthenticated", err)
	}
	return reported.Message()
}

func sessionMetadata(
	ctx context.Context, organization string, relay relayCredentials,
) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"opencluster-org-id", organization,
		"opencluster-registration-id", relay.registration.String(),
		"opencluster-relay-credential", relay.credential)
}

func awaitSessionAccepted(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.SessionAccepted {
	t.Helper()
	return awaitMessage(t, stream, "session acceptance",
		func(message *relayv1.ControlToRelay) *relayv1.SessionAccepted {
			return message.GetSessionAccepted()
		})
}

func awaitAssignment(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.JobAssignment {
	t.Helper()
	return awaitMessage(t, stream, "job assignment",
		func(message *relayv1.ControlToRelay) *relayv1.JobAssignment {
			return message.GetJobAssignment()
		})
}

func awaitResultAck(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.ResultAck {
	t.Helper()
	return awaitMessage(t, stream, "result acknowledgement",
		func(message *relayv1.ControlToRelay) *relayv1.ResultAck {
			return message.GetResultAck()
		})
}

func awaitReconnectInstruction(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) (*relayv1.GracefulReconnect, error) {
	t.Helper()

	var told *relayv1.GracefulReconnect
	for {
		message, err := stream.Recv()
		if err != nil {
			return told, err
		}
		if reconnect := message.GetGracefulReconnect(); reconnect != nil {
			told = reconnect
		}
	}
}

func awaitCancellation(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.Cancellation {
	t.Helper()
	return awaitMessage(t, stream, "cancellation",
		func(message *relayv1.ControlToRelay) *relayv1.Cancellation {
			return message.GetCancellation()
		})
}

func awaitMessage[T any](
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	wanted string,
	extract func(*relayv1.ControlToRelay) *T,
) *T {
	t.Helper()

	for {
		message, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for a %s: %v", wanted, err)
		}
		if found := extract(message); found != nil {
			return found
		}
	}
}

func sendResult(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient, job string, epoch uint64,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:      job,
			LeaseEpoch: epoch,
			Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
					KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
						Outcome:  relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
						Complete: true,
					},
				},
			}},
			ExecutionDurationMs: 12,
		},
	}})
	if err != nil {
		t.Fatalf("sending the result for job %s: %v", job, err)
	}
}

func acknowledgeCancellation(
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	cancellation *relayv1.Cancellation,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_CancelAck{
		CancelAck: &relayv1.CancelAck{
			JobId:       cancellation.GetJobId(),
			LeaseEpoch:  cancellation.GetLeaseEpoch(),
			Disposition: relayv1.CancelAck_DISPOSITION_ABORTED,
		},
	}})
	if err != nil {
		t.Fatalf("acknowledging the cancellation: %v", err)
	}
}

func sendCancelledResult(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient, job string, epoch uint64,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:      job,
			LeaseEpoch: epoch,
			Outcome: &relayv1.JobResult_Failure{Failure: &relayv1.JobFailure{
				Kind: relayv1.JobFailure_KIND_CANCELLED,
			}},
		},
	}})
	if err != nil {
		t.Fatalf("sending the cancelled result for job %s: %v", job, err)
	}
}

func workloadArguments(workload string) []byte {
	encoded, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
				Namespace:    "production",
				WorkloadKind: relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
				WorkloadName: workload,
				MaxPods:      50,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func enqueueJob(
	t *testing.T,
	database *storage.Database,
	organization uuid.UUID,
	registration uuid.UUID,
	arguments []byte,
) uuid.UUID {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	job := storage.RelayJob{
		ID:                uuid.New(),
		IntegrationID:     kubernetesIntegration(t, database, organization, registration),
		RegistrationID:    registration,
		CapabilityID:      capabilityUnderTest,
		CapabilityVersion: capabilityVersionUnderTest,
		Arguments:         arguments,
	}
	err := database.EnqueueVerifiedJob(ctx, organization, job)
	if err != nil {
		t.Fatalf("enqueueing a verified job: %v", err)
	}
	return job.ID
}

func kubernetesIntegration(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, registration uuid.UUID,
) uuid.UUID {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acting := ownerOf(t, organization)
	created, err := database.CreateIntegration(ctx, acting, organization, integrations.NewIntegration{
		Provider: "kubernetes",
		Name:     "cluster " + uuid.NewString(),
		RelayID:  registration,
	})
	if err != nil {
		t.Fatalf("creating a kubernetes integration: %v", err)
	}
	if _, err = database.RecordIntegrationVerification(ctx, acting, organization, created.ID,
		integrations.Verification{
			Status: integrations.StatusVerified,
			Grants: []string{capabilityUnderTest},
		}); err != nil {
		t.Fatalf("verifying the kubernetes integration: %v", err)
	}
	return created.ID
}

func namedOrganization(t *testing.T, organization string) uuid.UUID {
	t.Helper()

	named, err := uuid.Parse(organization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return named
}

func ownerOf(t *testing.T, organization uuid.UUID) authz.Principal {
	t.Helper()

	principal, err := authz.NewPrincipal(uuid.New(), uuid.New(), "Test Harness", "",
		authz.Membership{Organization: organization, Role: authz.Admin})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

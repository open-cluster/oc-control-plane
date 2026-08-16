package main

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

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Everything the session tests use to act as a relay: enrolling one, opening its stream,
// saying what it is, and reading what it is told. Kept apart from the tests so that what a
// test asserts stays legible without the plumbing that gets it there.

// The capability the test dispatches. Its version travels on the assignment because the
// relay picks an implementation by it.
const (
	capabilityUnderTest        = "kubernetes.workload.runtime"
	capabilityVersionUnderTest = 1
	protocolVersionUnderTest   = 1
)

// relayCredentials is a registered relay's durable identity, as the relay itself holds it.
type relayCredentials struct {
	registration uuid.UUID
	credential   string
}

// registerRelay enrols a relay the way one really enrols, through the registration service,
// so the session is authenticated against a credential the control plane actually issued.
func registerRelay(
	t *testing.T, connection *grpc.ClientConn, dsn, organization string,
) relayCredentials {
	t.Helper()

	token := "bootstrap-token-for-" + uuid.NewString()
	issueBootstrapToken(t, dsn, organization, token)

	client := relayv1.NewRelayRegistrationServiceClient(connection)
	response, err := register(t, client, organization, token, &relayv1.RegisterRequest{
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0-test",
		ClusterFingerprint: "kube-system-uid-under-test",
		Capabilities: []*relayv1.CapabilityDescriptor{
			{CapabilityId: capabilityUnderTest, CapabilityVersion: capabilityVersionUnderTest},
		},
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

// connectSession opens a stream for a relay carrying nothing over from a previous one.
func connectSession(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()
	return connectSessionDeclaring(t, connection, organization, relay, nil)
}

// connectSessionDeclaring opens the stream, says hello as a real relay does, and leaves it
// open for the duration of the test. The hello is what opens delivery, so a client that
// skipped it would be testing a session no relay ever has.
//
// The deadline bounds a failure: without it a message that never arrives hangs until the whole
// suite times out, which reports nothing about which message was missing.
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

// openStream opens the session without saying anything on it, for the tests whose subject is
// the hello itself.
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

// refuseSession opens a session that must not be accepted and returns the refusal message.
// A stream reports its refusal on the first receive rather than when it is opened, because
// the server has said nothing until then.
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

// awaitReconnectInstruction drains a session that is being closed, returning the reconnect
// instruction if one arrived before the end and the status that ended it.
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

// awaitMessage reads until the wanted kind arrives, ignoring the rest. Skipping rather than
// failing on an unexpected kind is deliberate: the server may add messages a real relay
// tolerates, and a test that breaks on a new heartbeat is testing the wire order instead of
// the guarantee.
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

// sendResult reports a successful execution. The epoch is echoed rather than remembered
// server-side, which is what makes a result from a superseded lease refusable.
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

// acknowledgeCancellation reports that the stop was processed. It changes nothing durable —
// the outcome still arrives as a result — so it is sent here mainly to prove that it does not.
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

// sendCancelledResult reports that an execution stopped because it was asked to. A cancelled
// job is a failure with a reason, not an absence of an outcome.
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

// workloadArguments encodes a capability argument as the planner will: already reduced to
// the wire form, so nothing has to interpret it between here and the relay.
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
	placements *storage.Placements,
	organization tenancy.Organization,
	registration uuid.UUID,
	arguments []byte,
) uuid.UUID {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	job := storage.Job{
		ID:                uuid.New(),
		IntegrationID:     kubernetesIntegration(t, placements, organization, registration),
		RegistrationID:    registration,
		CapabilityID:      capabilityUnderTest,
		CapabilityVersion: capabilityVersionUnderTest,
		Arguments:         arguments,
	}
	refusal, err := placements.EnqueueJob(ctx, organization, job)
	if err != nil {
		t.Fatalf("enqueueing a job: %v (%s)", err, refusal)
	}
	return job.ID
}

// kubernetesIntegration creates a Kubernetes Integration served by this relay. Every job
// names one: the Integration is what the job reaches, and the relay is where it runs.
func kubernetesIntegration(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, registration uuid.UUID,
) uuid.UUID {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acting := ownerOf(t, organization)
	created, err := placements.CreateIntegration(ctx, acting, organization, integrations.NewIntegration{
		Type:    integrations.TypeKubernetes,
		Name:    "cluster " + uuid.NewString(),
		RelayID: registration,
	})
	if err != nil {
		t.Fatalf("creating a kubernetes integration: %v", err)
	}
	return created.ID
}

func namedOrganization(t *testing.T, organization string) tenancy.Organization {
	t.Helper()

	named, err := tenancy.NewOrganization(organization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return named
}

// ownerOf is the principal a harness acts as when it arranges state through the store rather
// than through the surface. Every operator-facing store function takes one, because the tenancy
// boundary is checked in storage as well as in the authorization middleware.
func ownerOf(t *testing.T, organization tenancy.Organization) authz.Principal {
	t.Helper()

	principal, err := authz.NewPrincipal(authz.KindUser, "harness", "Test Harness",
		[]authz.Membership{{Organization: organization, Role: authz.Admin}})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

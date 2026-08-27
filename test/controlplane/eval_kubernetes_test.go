package controlplane

import (
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/test/eval"
)

// startEvalKubernetes composes the cluster fixture through the real Relay protocol. Evaluation
// tool calls therefore cross the same database job queue and typed wire boundary as production.
func startEvalKubernetes(t *testing.T, world *integrationPlane, one evalCase) {
	t.Helper()
	advertised := []string{
		capability.KubernetesWorkloadRuntime,
		capability.KubernetesNamespaceEvents,
		capability.KubernetesContainerLogs,
	}
	connection := dialRelay(t, world.relayAt)
	registration := registerRelay(t, connection, world.dsn, surfaceOrg, advertised...)
	world.relay = registration
	stream := openStream(t, connection, surfaceOrg, registration)
	descriptors := make([]*relayv1.CapabilityDescriptor, 0, len(advertised))
	for _, name := range advertised {
		descriptors = append(descriptors, &relayv1.CapabilityDescriptor{
			CapabilityId: name, CapabilityVersion: capability.SchemaVersion1,
		})
	}
	if err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_Hello{
		Hello: &relayv1.Hello{
			ProtocolVersion: protocolVersionUnderTest, RelayVersion: "eval-fixture",
			MaxConcurrentJobs: 4, Capabilities: descriptors,
		},
	}}); err != nil {
		t.Fatalf("declaring evaluation Kubernetes reads: %v", err)
	}
	awaitSessionAccepted(t, stream)

	namespaces := make(map[string]bool)
	for _, workload := range one.Kubernetes.Workloads {
		namespaces[workload.Namespace] = true
	}
	allowList := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		allowList = append(allowList, namespace)
	}
	sort.Strings(allowList)
	status, body := world.call(t, http.MethodPost, world.base(surfaceOrg)+"/integrations",
		map[string]any{
			"type": "kubernetes", "name": "Evaluation Kubernetes",
			"relayId":       registration.registration.String(),
			"configuration": map[string]any{"namespaceAllowList": strings.Join(allowList, ",")},
		})
	if status != http.StatusCreated {
		t.Fatalf("creating the evaluation Kubernetes Integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	status, body = world.call(t, http.MethodPost,
		world.base(surfaceOrg)+"/integrations/"+created.Integration.ID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying the evaluation Kubernetes Integration = %d: %s", status, body)
	}

	go serveEvalKubernetes(stream, one.Kubernetes)
}

type evalRelayStream interface {
	Send(*relayv1.RelayToControl) error
	Recv() (*relayv1.ControlToRelay, error)
}

func serveEvalKubernetes(stream evalRelayStream, fixture eval.Kubernetes) {
	for {
		message, err := stream.Recv()
		if err != nil {
			return
		}
		assignment := message.GetJobAssignment()
		if assignment == nil {
			continue
		}
		result := evalKubernetesResult(assignment, fixture)
		if err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
			JobResult: &relayv1.JobResult{
				JobId: assignment.GetJobId(), LeaseEpoch: assignment.GetLeaseEpoch(),
				Outcome: &relayv1.JobResult_Result{Result: result},
			},
		}}); err != nil {
			return
		}
	}
}

func evalKubernetesResult(
	assignment *relayv1.JobAssignment, fixture eval.Kubernetes,
) *relayv1.CapabilityResult {
	now := timestamppb.New(time.Now().UTC().Add(-20 * time.Minute))
	switch assignment.GetCapabilityId() {
	case capability.KubernetesWorkloadRuntime:
		arguments := assignment.GetArguments().GetKubernetesWorkloadRuntimeV1()
		workload := matchingEvalWorkload(fixture.Workloads, arguments.GetNamespace(),
			arguments.GetWorkloadName())
		return &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
				Outcome: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
				Workload: &relayv1.KubernetesWorkloadSummary{
					Kind: strings.ToLower(workload.Kind), Name: workload.Name,
					Namespace: workload.Namespace, DesiredReplicas: 2, ReadyReplicas: 2,
					UpdatedReplicas: 2, AvailableReplicas: 2,
					ContainerImages: []string{"payments:v2.13.9"},
				},
				Pods: []*relayv1.KubernetesPodRuntime{{
					Name: workload.Name + "-0", Phase: "Running", Ready: true, StartedAt: now,
				}},
				ReturnedPodCount: 1, Complete: true, ReadAt: now, AppliedMaxPods: 20,
			},
		}}
	case capability.KubernetesNamespaceEvents:
		return &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
				Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
				Events: []*relayv1.KubernetesEvent{{
					Type: "Warning", Reason: "Unhealthy",
					Message: "checkout dependency timed out", LastSeenAt: now, Count: 3,
				}},
				ReturnedEventCount: 1, Complete: true,
				AppliedRetentionHorizon: durationpb.New(time.Hour), ReadAt: now,
				AppliedMaxEvents: 100,
			},
		}}
	case capability.KubernetesContainerLogs:
		content := "upstream checkout dependency timeout; database pool remains available"
		return &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
				Outcome:           relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
				Lines:             []*relayv1.KubernetesLogLine{{At: now, Content: content}},
				ReturnedLineCount: 1, ReturnedByteCount: int64(len(content)), Complete: true,
				ReadAt: now, AppliedMaxLines: 200, AppliedMaxBytes: 64 << 10,
			},
		}}
	default:
		return &relayv1.CapabilityResult{}
	}
}

func matchingEvalWorkload(workloads []eval.Workload, namespace, name string) eval.Workload {
	for _, workload := range workloads {
		if workload.Namespace == namespace && workload.Name == name {
			return workload
		}
	}
	return eval.Workload{Kind: "Deployment", Name: name, Namespace: namespace}
}

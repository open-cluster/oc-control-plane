package capability_test

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/capability"
)

// The registry is what stops a job leaving this control plane that no Relay could serve and no
// investigator could use. It is the control plane's HALF of "neither side trusts the other" —
// the Relay re-validates everything on receipt, and these tests exist so that it is never the
// only thing that does.

var window = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

func encode(t *testing.T, arguments *relayv1.CapabilityArguments) []byte {
	t.Helper()
	encoded, err := proto.Marshal(arguments)
	if err != nil {
		t.Fatalf("encoding arguments: %v", err)
	}
	return encoded
}

func workload(namespace, name string, maxPods uint32) *relayv1.CapabilityArguments {
	return &relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
				Namespace:    namespace,
				WorkloadKind: relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
				WorkloadName: name,
				MaxPods:      maxPods,
			},
		},
	}
}

func events(namespace string, start, end time.Time, maxEvents uint32) *relayv1.CapabilityArguments {
	return &relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{
				Namespace:   namespace,
				WindowStart: timestamppb.New(start),
				WindowEnd:   timestamppb.New(end),
				MaxEvents:   maxEvents,
			},
		},
	}
}

func logs(namespace, pod, container string, maxLines, maxBytes uint32) *relayv1.CapabilityArguments {
	return &relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{
				Namespace:     namespace,
				PodName:       pod,
				ContainerName: container,
				MaxLines:      maxLines,
				MaxBytes:      maxBytes,
			},
		},
	}
}

func TestValidate_AcceptsEachCompiledCapabilityAtItsFrozenVersion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		id        string
		arguments *relayv1.CapabilityArguments
	}{
		"workload runtime": {capability.KubernetesWorkloadRuntime, workload("shop", "checkout", 10)},
		"namespace events": {capability.KubernetesNamespaceEvents,
			events("shop", window.Add(-time.Hour), window, 50)},
		"container logs": {capability.KubernetesContainerLogs,
			logs("shop", "checkout-7f", "api", 100, 65536)},
	} {
		if err := capability.Validate(tc.id, 1, encode(t, tc.arguments)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidate_RefusesAVersionThisBuildDoesNotDispatch(t *testing.T) {
	t.Parallel()
	// Schema versions are frozen, so v2 means semantics this build does not have. Sending it
	// anyway and letting the Relay refuse would cost a lease and a round trip to learn what is
	// knowable here — and the one failure worse than either is a Relay that has v2 and answers
	// under semantics this side cannot read.
	err := capability.Validate(capability.KubernetesContainerLogs, 2,
		encode(t, logs("shop", "checkout-7f", "api", 100, 65536)))
	if !errors.Is(err, capability.ErrUnknownCapability) {
		t.Fatalf("a version this build does not dispatch = %v, want ErrUnknownCapability", err)
	}
}

func TestValidate_RefusesACapabilityThatDoesNotExist(t *testing.T) {
	t.Parallel()
	if err := capability.Validate("kubernetes.node.shell", 1, nil); !errors.Is(
		err, capability.ErrUnknownCapability) {
		t.Fatalf("the registry is closed; got %v", err)
	}
}

func TestValidate_RefusesArgumentsForADifferentCapability(t *testing.T) {
	t.Parallel()
	// Perfectly valid log arguments, dispatched under the events capability. Nothing about the
	// bytes is wrong; what is wrong is that they answer a different question, and a Relay
	// executing them would return a result nothing could interpret.
	err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, logs("shop", "checkout-7f", "api", 100, 65536)))
	if !errors.Is(err, capability.ErrInvalidArguments) {
		t.Fatalf("mismatched arguments = %v, want ErrInvalidArguments", err)
	}
}

func TestValidate_RefusesArgumentsThatDoNotDecode(t *testing.T) {
	t.Parallel()
	if err := capability.Validate(capability.KubernetesWorkloadRuntime, 1,
		[]byte{0xff, 0xff, 0xff}); !errors.Is(err, capability.ErrInvalidArguments) {
		t.Fatalf("undecodable arguments = %v, want ErrInvalidArguments", err)
	}
}

func TestValidate_RefusesIdentifiersKubernetesCouldNotHave(t *testing.T) {
	t.Parallel()
	for name, arguments := range map[string]*relayv1.CapabilityArguments{
		"uppercase namespace": workload("Shop", "checkout", 10),
		"empty namespace":     workload("", "checkout", 10),
		"spaced workload":     workload("shop", "check out", 10),
		"empty pod":           logs("shop", "", "api", 10, 4096),
		"empty container":     logs("shop", "checkout-7f", "", 10, 4096),
		"uppercase container": logs("shop", "checkout-7f", "API", 10, 4096),
	} {
		id := capability.KubernetesWorkloadRuntime
		if arguments.GetKubernetesContainerLogsV1() != nil {
			id = capability.KubernetesContainerLogs
		}
		if err := capability.Validate(id, 1, encode(t, arguments)); !errors.Is(
			err, capability.ErrInvalidArguments) {
			t.Errorf("%s = %v, want ErrInvalidArguments", name, err)
		}
	}
}

func TestValidate_RefusesAnUnboundedOrInvertedEventWindow(t *testing.T) {
	t.Parallel()

	missing := events("shop", window.Add(-time.Hour), window, 50)
	missing.GetKubernetesNamespaceEventsV1().WindowStart = nil
	if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, missing)); !errors.Is(err, capability.ErrInvalidArguments) {
		t.Errorf("an absent window bound must be refused, not read as the beginning of time; got %v", err)
	}

	inverted := events("shop", window, window.Add(-time.Hour), 50)
	if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, inverted)); !errors.Is(err, capability.ErrInvalidArguments) {
		t.Errorf("a window that ends before it starts = %v, want ErrInvalidArguments", err)
	}

	point := events("shop", window, window, 50)
	if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, point)); !errors.Is(err, capability.ErrInvalidArguments) {
		t.Errorf("an empty window = %v, want ErrInvalidArguments", err)
	}

	wide := events("shop", window.Add(-30*24*time.Hour), window, 50)
	if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, wide)); !errors.Is(err, capability.ErrInvalidArguments) {
		t.Errorf("a window wider than the read serves = %v, want ErrInvalidArguments", err)
	}
}

// A bound above what the schema serves is refused rather than lowered. An operator reading the
// effective bound in a result should find the number they asked for or a refusal, and never a
// third value nobody chose.
func TestValidate_RefusesBoundsAboveWhatTheSchemaServes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		id        string
		arguments *relayv1.CapabilityArguments
	}{
		"pods":      {capability.KubernetesWorkloadRuntime, workload("shop", "checkout", capability.MaxPods+1)},
		"events":    {capability.KubernetesNamespaceEvents, events("shop", window.Add(-time.Hour), window, capability.MaxEvents+1)},
		"log lines": {capability.KubernetesContainerLogs, logs("shop", "p", "api", capability.MaxLines+1, 4096)},
		"log bytes": {capability.KubernetesContainerLogs, logs("shop", "p", "api", 10, capability.MaxBytes+1)},
	} {
		if err := capability.Validate(tc.id, 1, encode(t, tc.arguments)); !errors.Is(
			err, capability.ErrInvalidArguments) {
			t.Errorf("%s above the ceiling = %v, want ErrInvalidArguments", name, err)
		}
	}
}

// The narrowing is rendered into a Kubernetes field selector on the far side, so anything that
// could carry a separator into one is refused before it is sent. The Relay refuses it too;
// this is the half that stops it being sent at all.
func TestValidate_RefusesANarrowingThatCouldCarryASelectorSeparator(t *testing.T) {
	t.Parallel()
	for name, involved := range map[string]*relayv1.KubernetesInvolvedObject{
		"kind carrying a separator": {Kind: "Pod,involvedObject.name=x"},
		"name that is not a name":   {Name: "Checkout Pod"},
		"uid carrying an equals":    {Uid: "abc=123"},
	} {
		arguments := events("shop", window.Add(-time.Hour), window, 50)
		arguments.GetKubernetesNamespaceEventsV1().InvolvedObject = involved
		if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
			encode(t, arguments)); !errors.Is(err, capability.ErrInvalidArguments) {
			t.Errorf("%s = %v, want ErrInvalidArguments", name, err)
		}
	}
}

func TestValidate_AcceptsANarrowingBuiltFromIdentifiers(t *testing.T) {
	t.Parallel()
	arguments := events("shop", window.Add(-time.Hour), window, 50)
	arguments.GetKubernetesNamespaceEventsV1().InvolvedObject =
		&relayv1.KubernetesInvolvedObject{Kind: "Pod", Name: "checkout-7f", Uid: "abc-123"}

	if err := capability.Validate(capability.KubernetesNamespaceEvents, 1,
		encode(t, arguments)); err != nil {
		t.Fatalf("a narrowing built from identifiers must be accepted: %v", err)
	}
}

func TestRegistered_IsTheClosedSetInAStableOrder(t *testing.T) {
	t.Parallel()
	want := []string{
		capability.KubernetesContainerLogs,
		capability.KubernetesNamespaceEvents,
		capability.KubernetesWorkloadRuntime,
	}
	for attempt := range 5 {
		got := capability.Registered()
		if len(got) != len(want) {
			t.Fatalf("attempt %d: %d capabilities, want %d", attempt, len(got), len(want))
		}
		for index, id := range want {
			if got[index].ID != id || got[index].Version != 1 {
				t.Fatalf("attempt %d position %d = %s v%d, want %s v1",
					attempt, index, got[index].ID, got[index].Version, id)
			}
		}
	}
	if capability.Known(capability.KubernetesContainerLogs, 2) {
		t.Error("the registry must not claim a version it does not carry")
	}
}

package investigation

import (
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/capability"
)

// Arguments is a typed, bounded description of one read. It is the ONLY shape a planner can ask
// for anything in: there is no command string, no selector, no query and no free text, so the
// closed typed capability registry stays the only execution surface and adaptivity does not widen
// it (ADR-009).
//
// Fields that do not apply to a capability are ignored by its encoder rather than refused, because
// a planner filling in a field the read has no use for has made a harmless mistake and refusing it
// would turn that into a lost round.
type Arguments struct {
	Namespace     string
	WorkloadKind  WorkloadKind
	WorkloadName  string
	PodName       string
	ContainerName string
	// Previous asks for the log of the container instance BEFORE the current one. It is what
	// answers "what did it say before it died", which is the single most decisive artifact in a
	// crashloop and the reason ADR-012 lets bounded raw text reach the model at all.
	Previous  bool
	Window    Window
	MaxPods   uint32
	MaxEvents uint32
	MaxLines  uint32
	MaxBytes  uint32
}

// workloadKinds maps this package's vocabulary onto the protocol's. It is a table rather than a
// cast so that a value added on one side and not the other is a missing entry rather than a
// silently wrong read.
var workloadKinds = map[WorkloadKind]relayv1.WorkloadKind{
	WorkloadDeployment:  relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
	WorkloadStatefulSet: relayv1.WorkloadKind_WORKLOAD_KIND_STATEFULSET,
	WorkloadDaemonSet:   relayv1.WorkloadKind_WORKLOAD_KIND_DAEMONSET,
}

// Encode renders arguments as the capability message that travels. It returns the encoded bytes
// because that is the form they are stored and dispatched in: encoding after validation would
// leave the encoding unchecked, which is the half that actually goes on the wire.
func (a Arguments) Encode(capabilityID string) ([]byte, bool) {
	message := &relayv1.CapabilityArguments{}
	switch capabilityID {
	case capability.KubernetesWorkloadRuntime:
		message.Arguments = &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
				Namespace:    a.Namespace,
				WorkloadKind: workloadKinds[a.WorkloadKind],
				WorkloadName: a.WorkloadName,
				MaxPods:      bounded(a.MaxPods, capability.MaxPods),
			},
		}
	case capability.KubernetesNamespaceEvents:
		message.Arguments = &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{
				Namespace:   a.Namespace,
				WindowStart: timestamppb.New(a.Window.Start),
				WindowEnd:   timestamppb.New(a.Window.End),
				MaxEvents:   bounded(a.MaxEvents, capability.MaxEvents),
			},
		}
	case capability.KubernetesContainerLogs:
		message.Arguments = &relayv1.CapabilityArguments_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{
				Namespace:     a.Namespace,
				PodName:       a.PodName,
				ContainerName: a.ContainerName,
				Previous:      a.Previous,
				MaxLines:      bounded(a.MaxLines, capability.MaxLines),
				MaxBytes:      bounded(a.MaxBytes, capability.MaxBytes),
			},
		}
	default:
		return nil, false
	}

	encoded, err := proto.Marshal(message)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// bounded resolves what to ask for. A planner that named nothing gets the capability's ceiling
// rather than zero, and one that asked for more than the schema serves is lowered here — the
// alternative is a refusal for something no operator chose and no reader would understand.
func bounded(asked, ceiling uint32) uint32 {
	if asked == 0 || asked > ceiling {
		return ceiling
	}
	return asked
}

// openingReads is the deterministic plan every round begins with, and it is the whole of what this
// slice hard-codes about where to look.
//
// It is orientation rather than evidence selection: it establishes what the workload is, what the
// cluster said about it, and nothing else. What to read NEXT is the planner's, which is the
// distinction ADR-009 exists to keep — a fixed bundle of evidence would make this a runbook
// executor and would have the harness answer whether a predetermined bundle contained the answer.
func openingReads(scope Scope, window Window) []PlannedRead {
	return []PlannedRead{
		{
			CapabilityID:      capability.KubernetesWorkloadRuntime,
			CapabilityVersion: capability.SchemaVersion1,
			Purpose: "establish the workload's identity as the cluster reports it, including its " +
				"uid, and the runtime state of its pods",
		},
		{
			CapabilityID:      capability.KubernetesNamespaceEvents,
			CapabilityVersion: capability.SchemaVersion1,
			Purpose: "establish what the cluster said happened around this workload inside the " +
				"window, and whether the window reaches past what it can still answer for",
		},
	}
}

// openingProposals turns the opening plan into reads. They carry no justification because they
// precede every hypothesis, which is the one exemption from the rule that a read points at one.
func openingProposals(scope Scope, window Window) []Proposal {
	return []Proposal{
		{
			CapabilityID:      capability.KubernetesWorkloadRuntime,
			CapabilityVersion: capability.SchemaVersion1,
			Reason:            "the deterministic opening: what is being looked at",
			Arguments: Arguments{
				Namespace:    scope.Namespace,
				WorkloadKind: scope.WorkloadKind,
				WorkloadName: scope.WorkloadName,
			},
		},
		{
			CapabilityID:      capability.KubernetesNamespaceEvents,
			CapabilityVersion: capability.SchemaVersion1,
			Reason:            "the deterministic opening: what the cluster said, and when",
			Arguments: Arguments{
				Namespace: scope.Namespace,
				Window:    window,
			},
		},
	}
}

// availableCapabilities is what a planner may ask for, so it cannot propose a read that does not
// exist. It is the intersection of what this build dispatches and what local policy permits.
func availableCapabilities(controls Controls) []CapabilityRef {
	registered := capability.Registered()
	available := make([]CapabilityRef, 0, len(registered))
	for _, descriptor := range registered {
		if !controls.Permits(descriptor.ID) {
			continue
		}
		available = append(available, CapabilityRef{ID: descriptor.ID, Version: descriptor.Version})
	}
	return available
}

// eventHorizon is how far back Kubernetes Events can be relied on without the cluster saying
// otherwise. It is used only to phrase a gap's consequence when the Relay reports the window
// predates retention; the authority is always what the read came back with, never this constant.
const eventHorizon = time.Hour

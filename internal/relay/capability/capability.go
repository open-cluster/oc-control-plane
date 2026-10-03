package capability

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

const (
	KubernetesWorkloadRuntime = "kubernetes.workload.runtime"
	KubernetesNamespaceEvents = "kubernetes.namespace.events"
	KubernetesContainerLogs   = "kubernetes.container.logs"

	SchemaVersion1 = 1
)

const (
	MaxPods        = 50
	MaxEvents      = 200
	MaxLines       = 2000
	MaxBytes       = 256 * 1024
	MaxEventWindow = 7 * 24 * time.Hour
)

var ErrUnknownCapability = errors.New("capability is not one this build dispatches")

var ErrInvalidArguments = errors.New("capability arguments are not valid")

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	kindLiteral      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	uidLiteral       = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
)

type registered struct {
	id      string
	version uint32
}

var validators = map[registered]func(*relayv1.CapabilityArguments) error{
	{KubernetesWorkloadRuntime, SchemaVersion1}: validateWorkloadRuntime,
	{KubernetesNamespaceEvents, SchemaVersion1}: validateNamespaceEvents,
	{KubernetesContainerLogs, SchemaVersion1}:   validateContainerLogs,
}

func Known(id string, version uint32) bool {
	_, ok := validators[registered{id, version}]
	return ok
}

func Registered() []Descriptor {
	descriptors := make([]Descriptor, 0, len(validators))
	for key := range validators {
		descriptors = append(descriptors, Descriptor{ID: key.id, Version: key.version})
	}
	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].ID == descriptors[j].ID {
			return descriptors[i].Version < descriptors[j].Version
		}
		return descriptors[i].ID < descriptors[j].ID
	})
	return descriptors
}

type Descriptor struct {
	ID      string
	Version uint32
}

func Validate(id string, version uint32, arguments []byte) error {
	validate, ok := validators[registered{id, version}]
	if !ok {
		return fmt.Errorf("%w: %s v%d", ErrUnknownCapability, id, version)
	}

	decoded := &relayv1.CapabilityArguments{}
	if err := proto.Unmarshal(arguments, decoded); err != nil {
		return fmt.Errorf("%w: they do not decode", ErrInvalidArguments)
	}
	return validate(decoded)
}

func validateWorkloadRuntime(arguments *relayv1.CapabilityArguments) error {
	args := arguments.GetKubernetesWorkloadRuntimeV1()
	if args == nil {
		return fmt.Errorf("%w: they are not workload-runtime arguments", ErrInvalidArguments)
	}
	if args.GetWorkloadKind() == relayv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED {
		return fmt.Errorf("%w: the workload kind is unspecified", ErrInvalidArguments)
	}
	if !validNamespace(args.GetNamespace()) {
		return fmt.Errorf("%w: %q is not a namespace", ErrInvalidArguments, args.GetNamespace())
	}
	if !validObjectName(args.GetWorkloadName()) {
		return fmt.Errorf("%w: %q is not a workload name", ErrInvalidArguments, args.GetWorkloadName())
	}
	if args.GetMaxPods() > MaxPods {
		return fmt.Errorf("%w: max_pods %d is above the %d this schema serves",
			ErrInvalidArguments, args.GetMaxPods(), MaxPods)
	}
	return nil
}

func validateNamespaceEvents(arguments *relayv1.CapabilityArguments) error {
	args := arguments.GetKubernetesNamespaceEventsV1()
	if args == nil {
		return fmt.Errorf("%w: they are not namespace-events arguments", ErrInvalidArguments)
	}
	if !validNamespace(args.GetNamespace()) {
		return fmt.Errorf("%w: %q is not a namespace", ErrInvalidArguments, args.GetNamespace())
	}
	if args.GetWindowStart() == nil || args.GetWindowEnd() == nil {
		return fmt.Errorf("%w: the window must be bounded at both ends", ErrInvalidArguments)
	}
	start, end := args.GetWindowStart().AsTime(), args.GetWindowEnd().AsTime()
	if !start.Before(end) {
		return fmt.Errorf("%w: the window ends before it starts", ErrInvalidArguments)
	}
	if end.Sub(start) > MaxEventWindow {
		return fmt.Errorf("%w: the window is wider than the %s this read serves",
			ErrInvalidArguments, MaxEventWindow)
	}
	if args.GetMaxEvents() > MaxEvents {
		return fmt.Errorf("%w: max_events %d is above the %d this schema serves",
			ErrInvalidArguments, args.GetMaxEvents(), MaxEvents)
	}
	return validateNarrowing(args.GetInvolvedObject())
}

func validateNarrowing(involved *relayv1.KubernetesInvolvedObject) error {
	if involved == nil {
		return nil
	}
	if kind := involved.GetKind(); kind != "" &&
		(len(kind) > 63 || !kindLiteral.MatchString(kind)) {
		return fmt.Errorf("%w: %q is not a Kubernetes kind", ErrInvalidArguments, kind)
	}
	if name := involved.GetName(); name != "" && !validObjectName(name) {
		return fmt.Errorf("%w: %q is not an object name", ErrInvalidArguments, name)
	}
	if uid := involved.GetUid(); uid != "" &&
		(len(uid) > 253 || !uidLiteral.MatchString(uid)) {
		return fmt.Errorf("%w: %q is not a uid", ErrInvalidArguments, uid)
	}
	return nil
}

func validateContainerLogs(arguments *relayv1.CapabilityArguments) error {
	args := arguments.GetKubernetesContainerLogsV1()
	if args == nil {
		return fmt.Errorf("%w: they are not container-logs arguments", ErrInvalidArguments)
	}
	if !validNamespace(args.GetNamespace()) {
		return fmt.Errorf("%w: %q is not a namespace", ErrInvalidArguments, args.GetNamespace())
	}
	if !validObjectName(args.GetPodName()) {
		return fmt.Errorf("%w: %q is not a pod name", ErrInvalidArguments, args.GetPodName())
	}
	if !validNamespace(args.GetContainerName()) {
		return fmt.Errorf("%w: %q is not a container name", ErrInvalidArguments, args.GetContainerName())
	}
	if args.GetMaxLines() > MaxLines {
		return fmt.Errorf("%w: max_lines %d is above the %d this schema serves",
			ErrInvalidArguments, args.GetMaxLines(), MaxLines)
	}
	if args.GetMaxBytes() > MaxBytes {
		return fmt.Errorf("%w: max_bytes %d is above the %d this schema serves",
			ErrInvalidArguments, args.GetMaxBytes(), MaxBytes)
	}
	return nil
}

func validNamespace(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dns1123Label.MatchString(value)
}

func validObjectName(value string) bool {
	return len(value) > 0 && len(value) <= 253 && dns1123Subdomain.MatchString(value)
}

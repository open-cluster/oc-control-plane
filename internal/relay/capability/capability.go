// Package capability holds the control plane's side of the closed Relay Capability registry: which
// bounded reads exist, at which frozen schema versions, and what a valid argument for each one
// looks like.
//
// It exists so that neither half of the protocol trusts the other. The Relay re-validates
// every argument on receipt and refuses what it cannot serve; this is the same check made
// before anything is dispatched, so a job that could never have succeeded is refused here
// rather than after a round trip, and a Relay is never asked to be the only thing standing
// between a planner's mistake and a customer's cluster.
//
// The registry is closed by construction. There is no lookup in a table a message could add
// to, no dynamic registration, and no fall-through: a capability this build does not carry is
// refused, and so is a version of one it does.
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

// The capabilities this build can dispatch. Each entry is one frozen schema version: a
// semantic change mints a new version rather than editing an existing one, so the version is
// half the identity rather than a detail.
const (
	KubernetesWorkloadRuntime = "kubernetes.workload.runtime"
	KubernetesNamespaceEvents = "kubernetes.namespace.events"
	KubernetesContainerLogs   = "kubernetes.container.logs"

	SchemaVersion1 = 1
)

// Bounds the control plane will not dispatch beyond. They are the schema ceilings the Relay
// compiles, stated here so a request for more is refused before it is sent rather than
// silently lowered on arrival — an operator reading the effective bound in a result should
// find the number they asked for, or a refusal, and never a third value nobody chose.
const (
	MaxPods   = 50
	MaxEvents = 200
	MaxLines  = 2000
	MaxBytes  = 256 * 1024
	// MaxEventWindow bounds how far back an events read may reach. A window wider than this is
	// not a read, it is a request to page through a cluster's whole history one bounded page at
	// a time, and it would return an arbitrary subset while looking like a search.
	MaxEventWindow = 7 * 24 * time.Hour
)

// ErrUnknownCapability reports a capability, or a version of one, this build cannot dispatch.
var ErrUnknownCapability = errors.New("capability is not one this build dispatches")

// ErrInvalidArguments reports arguments that could not produce a usable read. It names what is
// wrong, because the caller is this control plane's own planner rather than an external party:
// there is no guess to protect here, and a refusal nobody can act on is a defect that will be
// rediscovered rather than fixed.
var ErrInvalidArguments = errors.New("capability arguments are not valid")

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	kindLiteral      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	uidLiteral       = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
)

// registered is the closed set, keyed by the whole identity.
type registered struct {
	id      string
	version uint32
}

var validators = map[registered]func(*relayv1.CapabilityArguments) error{
	{KubernetesWorkloadRuntime, SchemaVersion1}: validateWorkloadRuntime,
	{KubernetesNamespaceEvents, SchemaVersion1}: validateNamespaceEvents,
	{KubernetesContainerLogs, SchemaVersion1}:   validateContainerLogs,
}

// Known reports whether this build dispatches a capability at a version.
func Known(id string, version uint32) bool {
	_, ok := validators[registered{id, version}]
	return ok
}

// Registered lists what this build can dispatch, in a stable order, so an operator surface can
// say what exists rather than leaving a planner to guess a string.
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

// Descriptor is one capability at one frozen schema version.
type Descriptor struct {
	ID      string
	Version uint32
}

// Validate checks encoded arguments against the capability they claim to be for.
//
// The bytes are decoded here rather than taken as a typed message, because that is the form
// they are stored and dispatched in: validating a message and then encoding it would leave the
// encoding unchecked, which is the half that actually travels.
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
	// Both bounds are required. An absent one read as the beginning or end of time would turn a
	// bounded read into an unbounded one through an omission rather than a decision.
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

// validateNarrowing checks each field that was set. A field left empty narrows nothing; a field
// that was set must be an identifier, because it is about to be rendered into a Kubernetes
// field selector and an identifier is the only thing that cannot carry a separator into one.
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
	// Required. A pod's containers are not interchangeable, and an empty name would have the
	// Relay read whichever the spec happens to list first and report it as the one asked for.
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

// validNamespace also serves container names: both are DNS-1123 labels.
func validNamespace(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dns1123Label.MatchString(value)
}

func validObjectName(value string) bool {
	return len(value) > 0 && len(value) <= 253 && dns1123Subdomain.MatchString(value)
}

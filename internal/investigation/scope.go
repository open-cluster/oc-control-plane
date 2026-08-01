package investigation

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Scope resolution in this slice is deliberately narrow and hard-coded: one Kubernetes
// namespace, one workload kind, one workload name, reached through ONE Kubernetes Connection.
// It is not a general resource resolver and no canonical identity is involved.
//
// This code is expected to be discarded. What is owed instead of an early generalisation is a
// written list of what is hard-coded, and it is
// docs/architecture/hard-coded-in-the-first-investigation.md rather than a comment here, so that
// the next reader finds it without reading this file.

// WorkloadKind is what kind of Kubernetes workload an investigation is scoped to. The values
// match the protocol's WorkloadKind so the two cannot drift apart silently, and they are
// persisted, so they are frozen by a gate in internal/gates.
type WorkloadKind int16

const (
	WorkloadDeployment WorkloadKind = iota + 1
	WorkloadStatefulSet
	WorkloadDaemonSet
)

func (k WorkloadKind) String() string {
	switch k {
	case WorkloadDeployment:
		return "deployment"
	case WorkloadStatefulSet:
		return "statefulset"
	case WorkloadDaemonSet:
		return "daemonset"
	default:
		return "unrecognised"
	}
}

// ParseWorkloadKind reads what a caller named. It is deliberately exact rather than forgiving:
// a scope resolved from a value nobody typed is a scope nobody chose.
func ParseWorkloadKind(value string) (WorkloadKind, bool) {
	switch value {
	case "deployment":
		return WorkloadDeployment, true
	case "statefulset":
		return WorkloadStatefulSet, true
	case "daemonset":
		return WorkloadDaemonSet, true
	default:
		return 0, false
	}
}

// ErrScope reports a scope that could not be used. It names what is wrong, because the caller is
// an engineer starting an investigation and a refusal nobody can act on is a defect.
var ErrScope = errors.New("investigation scope is not usable")

// maxWindow bounds how far back an investigation may look. It is the widest window any capability
// in this build serves, so a wider one is a request every read would truncate silently.
const maxWindow = 7 * 24 * time.Hour

// minWindow keeps a window from being a point. A window that ends where it starts contains no
// events, and an investigation over it would abstain for a reason that was chosen rather than
// found.
const minWindow = time.Minute

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// Scope is what one Investigation looks at. The Connection is part of it rather than beside it:
// the Environment is DERIVED from the Connection and never accepted from a caller, so a scope
// that did not name one would have nothing to derive it from.
type Scope struct {
	// Connection is the Kubernetes Connection this investigation reads through, and the sole
	// authority for the Environment everything gathered under it belongs to.
	Connection   uuid.UUID
	Namespace    string
	WorkloadKind WorkloadKind
	WorkloadName string
}

// Validate refuses a scope no read could be made against.
func (s Scope) Validate() error {
	switch {
	case s.Connection == uuid.Nil:
		return fmt.Errorf("%w: it names no connection", ErrScope)
	case !validLabel(s.Namespace):
		return fmt.Errorf("%w: %q is not a namespace", ErrScope, s.Namespace)
	case s.WorkloadKind == 0:
		return fmt.Errorf("%w: the workload kind is unspecified", ErrScope)
	case !validObjectName(s.WorkloadName):
		return fmt.Errorf("%w: %q is not a workload name", ErrScope, s.WorkloadName)
	default:
		return nil
	}
}

// Window is when an investigation is looking at. Both ends are required: an absent one read as
// the beginning or the end of time turns a bounded investigation into an unbounded one through
// an omission rather than a decision.
type Window struct {
	Start time.Time
	End   time.Time
}

// Validate refuses a window no read could be bounded by.
func (w Window) Validate() error {
	switch {
	case w.Start.IsZero() || w.End.IsZero():
		return fmt.Errorf("%w: the window must be bounded at both ends", ErrScope)
	case !w.Start.Before(w.End):
		return fmt.Errorf("%w: the window ends before it starts", ErrScope)
	case w.End.Sub(w.Start) < minWindow:
		return fmt.Errorf("%w: the window is shorter than %s", ErrScope, minWindow)
	case w.End.Sub(w.Start) > maxWindow:
		return fmt.Errorf("%w: the window is wider than the %s any read here serves",
			ErrScope, maxWindow)
	default:
		return nil
	}
}

// Duration is how much time the window covers.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// Covers reports whether an instant falls inside the window, inclusive at both ends. Used to
// decide whether an observation the source timed belongs on this investigation's timeline.
func (w Window) Covers(at time.Time) bool {
	return !at.Before(w.Start) && !at.After(w.End)
}

func validLabel(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dns1123Label.MatchString(value)
}

func validObjectName(value string) bool {
	return len(value) > 0 && len(value) <= 253 && dns1123Subdomain.MatchString(value)
}

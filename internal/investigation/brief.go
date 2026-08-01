package investigation

import (
	"time"

	"github.com/google/uuid"
)

// Brief is the deterministic orientation assembled before any hypothesis exists: what is being
// looked at, when, what changed recently around it, what the live topology around it is, which
// capabilities are available in that Environment, and what coverage is missing.
//
// It states what may be asked and deliberately does not prescribe what will be. A fixed bundle of
// EVIDENCE would make this a runbook executor and would have the harness answer the wrong
// question — whether a predetermined bundle happened to contain the answer, rather than whether
// the system chooses useful next evidence (ADR-009).
//
// Assembling it never constitutes an investigation. A round that terminated at the end of its
// brief has abstained.
type Brief struct {
	Resource ResourceIdentity
	Trigger  Trigger
	Window   Window
	// RecentChanges is what moved around the resource inside the window. In this slice it is a
	// LIVE read rather than the change ledger (ADR-010), so where the source's own horizon
	// truncates the window, Coverage carries a gap saying so.
	RecentChanges []Change
	// Topology is the live context resolved per ADR-010 rather than read from a cache: owner
	// references and node placement. A relationship a conclusion depends on is revalidated live
	// and recorded as an Observation, never cited from a navigation index.
	Topology []TopologyFact
	// Available is what may be asked for in this Environment. The planner cannot propose a read
	// that does not exist, because it is given the list rather than a vocabulary.
	Available []CapabilityRef
	// Coverage is what is not reachable and why, per typed capability.
	Coverage    []Coverage
	AssembledAt time.Time
}

// CapabilityRef names one capability at one frozen schema version. Version is half the identity
// rather than a detail: a semantic change mints a new version rather than editing an existing
// one.
type CapabilityRef struct {
	ID      string `json:"id"`
	Version uint32 `json:"version"`
}

// ResourceIdentity is the resource as the CLUSTER reports it, including its UID, so a recreated
// object is not confused with its predecessor. Everything here came back from a read; nothing is
// echoed from what the caller asked for, because a brief that repeats the request cannot say the
// request was wrong.
type ResourceIdentity struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// UID is the cluster's own identity for this object. Without it, a workload deleted and
	// recreated inside the window reads as one continuous thing that briefly went unready.
	UID                string    `json:"uid"`
	DesiredReplicas    int32     `json:"desiredReplicas"`
	ReadyReplicas      int32     `json:"readyReplicas"`
	UpdatedReplicas    int32     `json:"updatedReplicas"`
	AvailableReplicas  int32     `json:"availableReplicas"`
	Generation         int64     `json:"generation"`
	ObservedGeneration int64     `json:"observedGeneration"`
	ContainerImages    []string  `json:"containerImages,omitempty"`
	CreatedAt          time.Time `json:"createdAt,omitzero"`
	// Resolved reports whether the cluster answered at all. A brief over an unresolved resource is
	// still assembled — what could not be read is the orientation in that case — and the planner
	// is told so rather than handed a zero value that looks like a healthy workload.
	Resolved bool `json:"resolved"`
}

// Change is something that moved around the resource inside the window.
//
// Every change carries the EvidenceItem that reports it. A change on the brief with no citation
// would be the one uncited statement in the case, and it would be the one an engineer acts on
// first.
type Change struct {
	At       time.Time `json:"at"`
	Summary  string    `json:"summary"`
	Evidence uuid.UUID `json:"evidence"`
}

// TopologyFact is one live relationship around the resource: which pod runs where, under which
// owner, in which state.
type TopologyFact struct {
	Pod   string `json:"pod"`
	Node  string `json:"node,omitempty"`
	Owner string `json:"owner,omitempty"`
	Phase string `json:"phase,omitempty"`
	Ready bool   `json:"ready"`
	// Evidence is the item this was read from, so topology is citable like everything else.
	Evidence uuid.UUID `json:"evidence"`
}

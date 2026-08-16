// Package changeledger owns the change ledger's vocabulary: the continuously persisted
// record of workload revisions and configuration changes. It is the only context this
// product persists continuously, because change history decays at its source — events
// expire, revision history is bounded, a ConfigMap edit leaves nothing behind — and
// cannot be recovered by a later read.
//
// The ledger is a navigation index, never citable. It tells an investigation what
// changed around a resource in a window; a conclusion resting on a change revalidates
// the current state live and cites the Observation that revalidation produced. A ledger
// entry is structurally refused as an EvidenceItem — it carries no capability read, no
// integration read and no trust class — so the rule holds by construction rather than by
// review.
//
// Persistence depends on this package and reconstructs its types; it declares none of
// them.
package changeledger

import (
	"time"

	"github.com/google/uuid"
)

// ObjectKind is the closed set of watched object kinds. Closed because what the ledger
// records is a decision, not an accident of what a cluster API returns; widening it is
// a contract change review sees, in the Relay's schema and here.
type ObjectKind int

const (
	KindDeployment  ObjectKind = 1
	KindStatefulSet ObjectKind = 2
	KindDaemonSet   ObjectKind = 3
	KindConfigMap   ObjectKind = 4
	KindSecret      ObjectKind = 5
)

func (k ObjectKind) String() string {
	switch k {
	case KindDeployment:
		return "deployment"
	case KindStatefulSet:
		return "statefulset"
	case KindDaemonSet:
		return "daemonset"
	case KindConfigMap:
		return "configmap"
	case KindSecret:
		return "secret"
	default:
		return "unknown"
	}
}

// ChangeKind says what an entry records. A baseline is where watching began, not a
// change: installing a Relay must not read as everything changing at once, so baseline
// entries are excluded from every change query and exist to give later diffs a visible
// starting point.
type ChangeKind int

const (
	ChangeBaseline ChangeKind = 1
	ChangeCreated  ChangeKind = 2
	ChangeModified ChangeKind = 3
	ChangeDeleted  ChangeKind = 4
)

func (k ChangeKind) String() string {
	switch k {
	case ChangeBaseline:
		return "baseline"
	case ChangeCreated:
		return "created"
	case ChangeModified:
		return "modified"
	case ChangeDeleted:
		return "deleted"
	default:
		return "unknown"
	}
}

// FieldChange is one watched field that moved, with both values, so "a deploy happened"
// reads as "the memory limit was halved". Values are identifiers, image references,
// quantities, name lists, versions and hashes — never content.
type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Change is one observed difference as a Relay reported it — the write model, before
// the ledger assigns identity.
type Change struct {
	Namespace        string
	Kind             ObjectKind
	Name             string
	UID              string
	ObservedRevision string
	Change           ChangeKind
	Fields           []FieldChange
}

// Delta is one at-least-once message from a Relay, reduced to domain terms. Its dedup
// key is derived from the Integration, each object's identity including UID, and the
// observed revision — never from the delta identifier, which only lets the Relay stop
// resending.
type Delta struct {
	IntegrationID  uuid.UUID
	PolicyRevision int64
	Baseline       bool
	// ObservedAt is when the Relay read the cluster, on the Relay's clock. Paired with
	// the recording clock so a delayed delivery is distinguishable from a delayed
	// change.
	ObservedAt time.Time
	Changes    []Change
}

// Entry is one durable ledger row.
type Entry struct {
	ID               int64
	IntegrationID    uuid.UUID
	Namespace        string
	Kind             ObjectKind
	Name             string
	UID              string
	ObservedRevision string
	Change           ChangeKind
	ObservedAt       time.Time
	RecordedAt       time.Time
	Fields           []FieldChange
}

// Summary renders an entry as the one-line statement a brief carries. Composed here,
// deterministically, from structured parts — never stored, and never authored by a
// Relay, so no free text from a customer's cluster can ride the ledger into a prompt.
func (e Entry) Summary() string {
	subject := e.Kind.String() + " " + e.Name
	switch e.Change {
	case ChangeCreated:
		return subject + " was created"
	case ChangeDeleted:
		return subject + " was deleted"
	case ChangeModified:
		if detail := summarizeFields(e.Fields); detail != "" {
			return subject + ": " + detail
		}
		return subject + " changed"
	default:
		return subject + " was first observed"
	}
}

// summarizeFields names up to three moved fields with their values. Three is enough to
// make the line actionable; the full itemization stays on the entry.
func summarizeFields(fields []FieldChange) string {
	const most = 3
	rendered := ""
	for i, field := range fields {
		if i == most {
			return rendered + ", and more"
		}
		if i > 0 {
			rendered += ", "
		}
		switch {
		case field.Before == "":
			rendered += field.Field + " set to " + field.After
		case field.After == "":
			rendered += field.Field + " removed (was " + field.Before + ")"
		default:
			rendered += field.Field + " " + field.Before + " -> " + field.After
		}
	}
	return rendered
}

// Scope is one Integration's synchronization state: what was requested, and the coverage
// boundaries an honest brief needs — where continuous knowledge starts, and when it was
// last confirmed current.
type Scope struct {
	IntegrationID     uuid.UUID
	PolicyRevision    int64
	RequestedInterval time.Duration
	// CoveredSince is where the ledger's CONTINUOUS knowledge of this scope begins. It
	// moves forward when a re-baseline finds anything changed across a gap in watching,
	// so a window opening before it is a window the ledger cannot vouch for.
	CoveredSince *time.Time
	// BaselineAt is the latest baseline observation. It never moves CoveredSince
	// backwards.
	BaselineAt *time.Time
	// LastConfirmedAt is the last instant the scope was confirmed current — a delta, a
	// baseline, or a heartbeat stamp reporting a completed tick.
	LastConfirmedAt *time.Time
	// Faulted reports that the Relay's most recent tick could not read the cluster; a
	// faulted scope is stale no matter what the stamp says.
	Faulted bool
	// Truncated reports that the most recent tick saw a bounded page rather than
	// everything, so changes in the unseen remainder are invisible.
	Truncated bool
}

// Freshness is a heartbeat's per-scope stamp: the Relay confirming a tick completed
// (or failed) without a delta having to travel.
type Freshness struct {
	IntegrationID  uuid.UUID
	PolicyRevision int64
	CompletedAt    *time.Time
	Faulted        bool
	Truncated      bool
}

// Recorded is what one delta's recording decided, for the session's log line.
type Recorded struct {
	// Inserted is how many entries were new; the rest collapsed against rows the ledger
	// already held, which is how an at-least-once redelivery records nothing twice.
	Inserted int
	// Refused reports a delta naming a Integration this Relay does not serve — recorded
	// nowhere, acknowledged anyway so the Relay stops resending what will never land.
	Refused bool
}

// WindowChanges answers the brief's one question: what changed around this resource,
// in this window — plus the coverage boundaries that keep an empty answer honest.
type WindowChanges struct {
	// Covered reports that the scope exists and has a baseline, so silence within the
	// boundaries below means "nothing changed" rather than "nobody was watching".
	Covered bool
	Scope   Scope
	// Entries are the window's changes, namespace-wide, oldest first, baselines
	// excluded, capped at the query's bound.
	Entries []Entry
	// Truncated reports that more changes existed than the cap.
	Truncated bool
}

// Package incident owns the operational incident AlertEvents group into.
//
// An Incident is one durable operational incident: the thing twenty notifications about one
// failure are twenty notifications ABOUT. It is provisional grouping and not causal truth, which
// is why it may be merged without anything being rewritten, and why every incident records the
// basis on which it was grouped.
//
// THE GROUPING IDENTITY IS THE SOURCE'S OWN. Nothing here is inferred from a AlertEvent's labels.
// Deriving an incident from what an alert says about a namespace or a pod would mean deciding that
// the object one system names one way and another names another are the same thing — canonical
// resource identity, which does not exist in this product and has one line of design behind it.
// What this uses instead is what the customer's own alerting already decided belonged together,
// and a source that supplies no such identity gets one incident per alert: a wrong split leaves a
// redundant record, and a wrong merge produces an investigation with an incoherent scope.
//
// This package declares its own Store and does not import persistence, so the capability owns its
// vocabulary and persistence depends on it (ADR-017). The routes live on the operator surface,
// which already owns who may reach them; this package owns what they mean.
package incident

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// The refusals this capability produces.
var (
	// ErrUnknown reports an incident this organization does not have. It is one answer for "no such
	// incident" and "that incident is another tenant's", because telling them apart would let a
	// caller compose path parameters until one of them landed.
	ErrUnknown = errors.New("incident incident unknown")
	// ErrMerge reports a merge that would not mean anything. It names which of the reasons applies,
	// because the caller is an operator correcting a grouping and a refusal nobody can act on is a
	// defect.
	ErrMerge = errors.New("these incidents cannot be merged")
	// ErrAlreadyInvestigated reports an incident that already has its Investigation. Repeated
	// notifications about one failure must not fragment into many cases, so the second request is
	// refused rather than quietly opening one.
	ErrAlreadyInvestigated = errors.New("this incident already has an investigation")
	// ErrBadCursor reports a resume point that did not come from a previous page. It is this
	// package's value rather than persistence's, so a handler can recognise it without importing
	// the layer that produced it (ADR-017).
	ErrBadCursor = errors.New("cursor is not a page position")
)

// Status is where an incident has got to. There are two: it is happening, or every AlertEvent in it
// stopped. Anything richer belongs to the Investigation attached to it.
//
// The values are persisted and frozen by a gate in test/architecture.
type Status int16

const (
	StatusOpen Status = iota + 1
	StatusResolved
)

func (s Status) String() string {
	switch s {
	case StatusOpen:
		return "open"
	case StatusResolved:
		return "resolved"
	default:
		return "unrecognised"
	}
}

// ParseStatus reads a status a caller asked to filter by. It is exact rather than forgiving: a
// filter resolved from a value nobody typed narrows a listing in a way nobody chose.
func ParseStatus(value string) (Status, bool) {
	switch value {
	case "open":
		return StatusOpen, true
	case "resolved":
		return StatusResolved, true
	default:
		return 0, false
	}
}

// Basis is WHO decided that the AlertEvents in an incident belong together. It is recorded so that a
// surprising grouping can be explained rather than argued about, which is the whole difference
// between a grouping that is a decision and one that is an accident of implementation.
//
// The values are persisted and frozen by a gate in test/architecture.
type Basis int16

const (
	// BasisSourceGrouping is the customer's own alerting saying so. Alertmanager computes a group
	// key from the group_by its operator wrote, and this platform takes it at face value.
	BasisSourceGrouping Basis = iota + 1
	// BasisUngrouped is a delivery that carried no grouping identity at all, so this alert is its
	// own incident. It is a first-class answer rather than a degraded one: grouping alerts nobody
	// grouped would be this platform inventing an incident.
	BasisUngrouped
)

func (b Basis) String() string {
	switch b {
	case BasisSourceGrouping:
		return "source_grouping"
	case BasisUngrouped:
		return "ungrouped"
	default:
		return "unrecognised"
	}
}

// Explain says in words why these AlertEvents are together, for an operator reading a grouping they
// did not expect. It is here rather than in the view layer because it is the meaning of the value
// and not a rendering of it.
func (b Basis) Explain() string {
	switch b {
	case BasisSourceGrouping:
		return "the source that delivered these alerts grouped them under one identity of its own"
	case BasisUngrouped:
		return "the source supplied no grouping identity, so this alert is an incident by itself"
	default:
		return "the basis for this grouping was not recorded"
	}
}

// Incident is one durable operational incident.
type Incident struct {
	ID           uuid.UUID
	Organization string
	// Integration is the installation the AlertEvents arrived through.
	Integration uuid.UUID
	// IntegrationName is what that installation is called, resolved by the read that
	// returned this incident. The identity is what a link is built from and this is what a
	// person reads, so both travel: a responder arriving from their own alerting wants to
	// know whether to go and look at Alertmanager or at something else.
	//
	// Empty where the name could not be resolved, which is the honest rendering of that
	// case rather than a placeholder. Unreachable through the API today, because deleting
	// an integration an incident references is refused.
	IntegrationName string
	// GroupingKey is the source's own identity for what belongs together. It is shown to an
	// operator because it is the answer to "why are these one incident", and it is untrusted text
	// like everything else a customer's system produced.
	GroupingKey string
	Basis       Basis
	Title       string
	Status      Status
	// FirstSeenAt and LastSeenAt are the SOURCE's clock at both ends. An incident's window is what
	// an investigation would be scoped to, so a delivery delay must not widen it.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// ResolvedAt is when the last AlertEvent in this incident stopped firing, and is zero while any is.
	ResolvedAt      time.Time
	AlertEventCount int
	// SupersededBy is the incident an operator merged this one into. Both records survive and
	// nothing is rewritten, so correcting a grouping does not destroy the record of having made
	// the original one.
	SupersededBy    *uuid.UUID
	SupersededAt    time.Time
	SupersedeReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Superseded reports whether an operator has merged this incident into another.
func (e Incident) Superseded() bool { return e.SupersededBy != nil }

// AlertEvent is one normalised alert as an incident's reader sees it.
//
// It is a projection rather than the intake record: what a reader of an incident needs is what
// each alert said and when, not the delivery machinery that brought it. Nothing here can say which
// system delivered it, which is the property that keeps a second Integration cheap.
type AlertEvent struct {
	ID    uuid.UUID
	Title string
	// Summary is free text from the customer's systems. Untrusted for its whole life: it may be
	// attacker-influenced and must never become an instruction, a destination or an authorisation
	// claim downstream.
	Summary string
	Labels  map[string]string
	// Firing reports whether this alert is still happening.
	Firing     bool
	StartedAt  time.Time
	ResolvedAt time.Time
	ReceivedAt time.Time
}

// Merge is one operator's correction of a grouping.
type Merge struct {
	// Absorbed is the incident that gives way. It keeps its identity, its AlertEvents and its record,
	// and gains a pointer to the one that survives.
	Absorbed uuid.UUID
	// Into is the incident that survives and is what a reader is shown.
	Into uuid.UUID
	// Reason is why an operator says these are one incident. It is required: a merge nobody
	// explained is a grouping decision a later reader cannot check, which is the thing recording
	// the basis exists to prevent in the automatic case.
	Reason string
}

// Validate refuses a merge that could not mean anything, before any row is read.
func (m Merge) Validate() error {
	switch {
	case m.Absorbed == uuid.Nil || m.Into == uuid.Nil:
		return errors.New("a merge names two incidents")
	case m.Absorbed == m.Into:
		return errors.New("an incident cannot be merged into itself")
	case len(m.Reason) == 0:
		return errors.New("a merge states why these are one incident")
	case len(m.Reason) > MaxReasonLength:
		return errors.New("the reason is longer than this record holds")
	default:
		return nil
	}
}

// MaxReasonLength matches the column. Checked here so an over-long reason is an answer rather than
// a constraint violation surfacing as a server error.
const MaxReasonLength = 1024

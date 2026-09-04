package incident

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnknown   = errors.New("incident unknown")
	ErrMerge     = errors.New("these incidents cannot be merged")
	ErrBadCursor = errors.New("cursor is not a page position")
)

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

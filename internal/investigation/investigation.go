package investigation

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Refusals opening or addressing a case can produce.
//
// There is deliberately no "spans two Environments" refusal. A scope names ONE Connection and the
// Environment is derived from it, so a case cannot be built that spans the boundary — the request
// has nowhere to put a second one. The specification asks for that request to be refused; making it
// unrepresentable is the stronger answer, and an error nothing can return would be a claim that
// something is checked when the check is the type system.
var (
	// ErrUnknown reports a case this organization does not have. It is one answer for "no such
	// case" and "that case is another tenant's", because telling them apart would let a caller
	// compose path parameters until one of them landed.
	ErrUnknown = errors.New("investigation unknown")
	// ErrConnectionUnusable reports a Connection that cannot carry an investigation: not this
	// organization's, disabled, trigger-only, or served by no Relay. One answer for all of them,
	// for the reason the storage layer already gives for a refused job.
	ErrConnectionUnusable = errors.New("connection cannot carry an investigation")
	// ErrAlreadyTerminal reports an act on a case that has finished. Cancelling one that already
	// concluded, for instance, which is not a failure and not a change.
	ErrAlreadyTerminal = errors.New("investigation is already terminal")
)

// TriggerKind is what started a case. There is one in this slice — a human naming a Connection,
// a scope and a window — and the enumeration exists because the next slice adds Signals and a
// column with one legal value is a column that gets read as decoration.
//
// The values are persisted and frozen by a gate in internal/gates.
type TriggerKind int16

const (
	// TriggerManual is an engineer who has been paged and has a namespace and a workload name.
	TriggerManual TriggerKind = iota + 1
	// TriggerSignal is an inbound SignalUpdate. Not reachable in this slice; declared so the
	// column means something when it becomes reachable.
	TriggerSignal
)

func (t TriggerKind) String() string {
	switch t {
	case TriggerManual:
		return "manual"
	case TriggerSignal:
		return "signal"
	default:
		return "unrecognised"
	}
}

// Trigger is what started a case and who asked. Attribution travels with it because the record
// has to be able to say who asked, and because an operator reading a cost figure needs to know
// whose it was.
type Trigger struct {
	Kind TriggerKind
	// RequestedBy is whoever started this. With one shared operator token this can say where a
	// request came from and not who made it — a limit of ADR-006 being undecided rather than of
	// this record, and worth knowing when reading it back.
	RequestedBy string
	At          time.Time
}

// Investigation is the durable case for one operational episode: one stable identity, one URL,
// one permalink, for the whole life of the failure. Reinvestigation adds a round to it and never
// creates a second one.
//
// CaseVersion is the load-bearing field. It advances whenever ANYTHING within the case changes —
// a round opening, an evidence item arriving, a gap being recorded, a hypothesis moving, an
// outcome reached or superseded — not only on a lifecycle transition. The frozen .NET frontend
// audit recorded the alternative and its consequence: a version tracking only lifecycle
// transitions left a polling client blind to the growth it was watching for. The database
// advances it, in triggers, so no write path can forget to.
type Investigation struct {
	ID           uuid.UUID
	Organization string
	// Environment is DERIVED from the Connection and persisted here rather than joined, so a
	// Connection that later moves cannot rewrite the scope a finished case was gathered under.
	Environment uuid.UUID
	// EpisodeKey names the IncidentEpisode this case belongs to. A manually started case uses an
	// implicit episode and leaves this empty; the field exists because the one-to-many between
	// case and round has to be structurally right from the first migration (ADR-013).
	EpisodeKey  string
	Scope       Scope
	Window      Window
	Trigger     Trigger
	Lifecycle   Lifecycle
	CaseVersion int64
	// CurrentRound is the ordinal of the round in progress, or of the last one to run.
	CurrentRound int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// TerminalAt is stamped when the case reaches a terminal lifecycle state, and is zero while
	// it has not.
	TerminalAt time.Time
}

// Terminal reports whether this case has finished.
func (i Investigation) Terminal() bool { return i.Lifecycle.Terminal() }

// New is what an engineer asked for. It travels as one value because the parts constrain each
// other: the Environment is derived from the Connection the scope names, so a scope and a
// Connection validated apart could be combined into a case that spans a boundary.
type New struct {
	Scope   Scope
	Window  Window
	Trigger Trigger
	// EpisodeKey is optional and empty for a manual start.
	EpisodeKey string
}

// Validate refuses a request no case could be opened from.
func (n New) Validate() error {
	if err := n.Scope.Validate(); err != nil {
		return err
	}
	if err := n.Window.Validate(); err != nil {
		return err
	}
	if n.Trigger.Kind == 0 {
		return errors.New("investigation trigger is unspecified")
	}
	return nil
}

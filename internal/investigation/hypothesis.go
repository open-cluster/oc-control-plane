package investigation

import (
	"time"

	"github.com/google/uuid"
)

// HypothesisState is where a proposed explanation has got to.
//
// The values are persisted and frozen by a gate in internal/gates.
type HypothesisState int16

const (
	// HypothesisLive is under active investigation and unresolved.
	HypothesisLive HypothesisState = iota + 1
	// HypothesisSupported has evidence behind it and nothing decisive against it.
	HypothesisSupported
	// HypothesisFalsified had its falsification condition met.
	HypothesisFalsified
	// HypothesisSetAside was not pursued, with the reason recorded. An alternative set aside
	// silently is an alternative a reader cannot check.
	HypothesisSetAside
)

func (s HypothesisState) String() string {
	switch s {
	case HypothesisLive:
		return "live"
	case HypothesisSupported:
		return "supported"
	case HypothesisFalsified:
		return "falsified"
	case HypothesisSetAside:
		return "set_aside"
	default:
		return "unrecognised"
	}
}

// Stance is how one EvidenceItem stands towards one Hypothesis. Contradiction is retained and
// shown rather than resolved silently in favour of the leading explanation (ADR-011).
//
// The values are persisted and frozen by a gate in internal/gates.
type Stance int16

const (
	StanceSupports Stance = iota + 1
	StanceContradicts
	// StanceNeutral is evidence that was considered and moved nothing. Recorded because the
	// record of what was tried is what tells a reviewer that a hypothesis was examined rather
	// than ignored.
	StanceNeutral
)

func (s Stance) String() string {
	switch s {
	case StanceSupports:
		return "supports"
	case StanceContradicts:
		return "contradicts"
	case StanceNeutral:
		return "neutral"
	default:
		return "unrecognised"
	}
}

// ParseStance reads a stance a client asked to filter by.
func ParseStance(value string) (Stance, bool) {
	switch value {
	case "supports":
		return StanceSupports, true
	case "contradicts":
		return StanceContradicts, true
	case "neutral":
		return StanceNeutral, true
	default:
		return 0, false
	}
}

// Hypothesis is a proposed explanation under active investigation, carrying a falsification
// condition. The condition is required rather than optional: an explanation nothing could
// disprove is a belief, and the whole apparatus below it exists to tell those apart.
type Hypothesis struct {
	ID      uuid.UUID
	RoundID uuid.UUID
	// Ordinal is the planner's ranking, exposed as an ORDINAL and never as a score. An internal
	// ranking may order these; publishing the number would give a reader a confidence figure the
	// read model deliberately does not have.
	Ordinal   int
	Statement string
	// Falsifies is what would disprove this. It is what a justified capability request points at,
	// and it is the reason evidence selection can be scored apart from the conclusion.
	Falsifies string
	State     HypothesisState
	// SetAsideReason is why this was not pursued, and is required when the state is set aside.
	SetAsideReason string
	// Pass is which call proposed it. Zero is the opening, from the brief alone; one and two are
	// the adaptive passes; one past the last pass is the conclusion. A number rather than a flag,
	// because the useful question later is not whether a hypothesis arrived late but how late —
	// one proposed at the conclusion has had no read dispatched against it and cannot have.
	Pass       int
	ProposedAt time.Time
	UpdatedAt  time.Time
}

// Weighed is one EvidenceItem's stance towards one Hypothesis, with the planner's reason for it.
type Weighed struct {
	HypothesisID uuid.UUID
	EvidenceID   uuid.UUID
	Stance       Stance
	// Reason is why the evidence stands that way. Without it a stance is an assertion, and the
	// point of persisting reasoning artifacts is that a surprising conclusion can be examined
	// rather than argued about.
	Reason     string
	RecordedAt time.Time
}

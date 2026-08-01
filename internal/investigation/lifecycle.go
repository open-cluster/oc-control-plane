package investigation

// Lifecycle is where an Investigation has got to, as a closed vocabulary.
//
//	pending → briefing → reasoning → gathering → (reasoning → gathering)* →
//	concluded | abstained | cancelled | failed
//
// It is a projection across the case's rounds rather than a second state machine beside them:
// the case is in the state its current round is executing in, and reinvestigation returns it to
// briefing rather than creating a second case (ADR-013).
//
// concluded and abstained are BOTH successful outcomes and are distinguished because they mean
// different things to whoever reads them. failed is reserved for the platform failing — the
// model provider unavailable, an execution limit reached before any round produced anything, a
// storage error —
// and never for "no explanation was found", which is abstained.
//
// The values are persisted and appear as literals in the SQL that reads them. They are frozen by
// a gate in internal/gates; changing one rewrites what every existing row means.
type Lifecycle int16

const (
	// LifecyclePending is a case whose first round has not been claimed by a worker yet.
	LifecyclePending Lifecycle = iota + 1
	// LifecycleBriefing is assembling the deterministic orientation. No hypothesis exists yet.
	LifecycleBriefing
	// LifecycleReasoning is the planner holding hypotheses and choosing what to ask for.
	LifecycleReasoning
	// LifecycleGathering is waiting on dispatched capability reads.
	LifecycleGathering
	// LifecycleConcluded is a terminal case carrying a most supported explanation.
	LifecycleConcluded
	// LifecycleAbstained is a terminal case that declined to conclude and said why. A first-class
	// outcome, not a failure.
	LifecycleAbstained
	// LifecycleCancelled is a terminal case someone stopped.
	LifecycleCancelled
	// LifecycleFailed is a terminal case the PLATFORM failed to run. Never "nothing was found".
	LifecycleFailed
)

func (l Lifecycle) String() string {
	switch l {
	case LifecyclePending:
		return "pending"
	case LifecycleBriefing:
		return "briefing"
	case LifecycleReasoning:
		return "reasoning"
	case LifecycleGathering:
		return "gathering"
	case LifecycleConcluded:
		return "concluded"
	case LifecycleAbstained:
		return "abstained"
	case LifecycleCancelled:
		return "cancelled"
	case LifecycleFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// Terminal reports whether nothing further will happen to this case without a new round.
func (l Lifecycle) Terminal() bool {
	switch l {
	case LifecycleConcluded, LifecycleAbstained, LifecycleCancelled, LifecycleFailed:
		return true
	default:
		return false
	}
}

// Running reports whether a worker currently holds this case. It is the distinction an operator
// needs between a quiet case and a stalled one, and it is why waiting is not the same as
// terminal.
func (l Lifecycle) Running() bool {
	switch l {
	case LifecycleBriefing, LifecycleReasoning, LifecycleGathering:
		return true
	default:
		return false
	}
}

// LifecycleFor maps a round's terminal outcome to the case state that projects it.
func LifecycleFor(outcome RoundOutcome) Lifecycle {
	switch outcome {
	case RoundConcluded:
		return LifecycleConcluded
	case RoundAbstained:
		return LifecycleAbstained
	case RoundCancelled:
		return LifecycleCancelled
	default:
		return LifecycleFailed
	}
}

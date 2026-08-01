package investigation

import (
	"time"

	"github.com/google/uuid"
)

// RoundOutcome is how one bounded execution ended. A round with no outcome is still running.
//
// The values are persisted and frozen by a gate in internal/gates.
type RoundOutcome int16

const (
	// RoundConcluded reached a most supported explanation, every claim cited.
	RoundConcluded RoundOutcome = iota + 1
	// RoundAbstained declined to conclude and named what was missing. A successful outcome.
	RoundAbstained
	// RoundCancelled was stopped by whoever started it.
	RoundCancelled
	// RoundFailed is the PLATFORM failing: the model provider unavailable, an execution limit reached
	// before this round produced anything, a storage error. Never "no explanation was found".
	RoundFailed
)

func (o RoundOutcome) String() string {
	switch o {
	case RoundConcluded:
		return "concluded"
	case RoundAbstained:
		return "abstained"
	case RoundCancelled:
		return "cancelled"
	case RoundFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// Round is one bounded, immutable execution of the investigator under pinned planner, model and
// policy versions. It owns the case pack: the closed-world record sufficient to replay it with no
// access to live sources or current state.
//
// It is leased exactly as relay_job is — a server-clock lease fenced by session and generation —
// because a control-plane restart must return a round to claimable and a worker that lost its
// lease must not be able to write. Inventing a second concurrency model for the same problem is
// the mistake ADR-004 warns about in another domain.
type Round struct {
	ID              uuid.UUID
	InvestigationID uuid.UUID
	// Ordinal is which round of this case it is, from one. It is what an export names when it
	// says which rounds it includes.
	Ordinal int
	// Brief is the deterministic orientation, pinned. It is assembled before any hypothesis
	// exists, and a round that terminated at the end of it has abstained rather than
	// investigated.
	Brief Brief
	// Controls is the fully resolved execution-control snapshot this round ran under, so "why
	// did this round stop after two requests" stays answerable without current configuration
	// (ADR-015).
	Controls Controls
	// Plan is the resolved evidence-plan snapshot: what this round MEANT to read, recoverable
	// without reading the source that produced it (ADR-009).
	Plan Plan
	// Versions pins the components that produced this round, so a stale recording is detected
	// rather than silently replayed.
	Versions Versions
	Outcome  RoundOutcome
	// Spend is what this round consumed. An operator cannot price a feature whose unit cost is
	// unknown, and a founder cannot enable one.
	Spend Spend

	LeaseSession uuid.UUID
	LeaseEpoch   int64
	StartedAt    time.Time
	TerminalAt   time.Time
}

// Running reports whether this round has not reached an outcome.
func (r Round) Running() bool { return r.Outcome == 0 }

// Fence identifies which execution of a round a write belongs to. Both halves are checked on
// every fenced write, so ownership is validated rather than inferred from who was last seen.
type Fence struct {
	RoundID      uuid.UUID
	LeaseSession uuid.UUID
	LeaseEpoch   int64
}

// Versions pins what produced a round. A key mismatch against a recorded transcript fails loudly
// rather than replaying a stale recording, which is the whole reason each of these is here rather
// than assumed.
type Versions struct {
	Planner       string
	Model         string
	PromptVersion string
	SchemaVersion string
	Investigator  string
}

// Spend is what a round consumed, recorded per round and summed per case.
type Spend struct {
	Requests    int
	ResultBytes int64
	// Tokens is what the model consumed. Zero is a legitimate answer for a replayed transcript,
	// and is not the same as unknown — a replayed round is free, and saying so is the point.
	Tokens int64
	// MicroCents keeps money in an integer. A float here would make two runs that cost the same
	// report different totals, which is the one property a cost figure has to have.
	MicroCents int64
	Duration   time.Duration
}

// Plan is the resolved evidence plan for one round: the declared set of capability calls the
// round intended to make, snapshotted so a run's intentions survive the code that produced them.
type Plan struct {
	// Template names the versioned plan this was resolved from. It is a string rather than an
	// identifier because it is a compiled artefact of this build and never a customer record.
	Template string
	// Intended is what the plan said to read, in order.
	Intended []PlannedRead
}

// PlannedRead is one capability call a plan intended, with the bounds it intended it under.
type PlannedRead struct {
	CapabilityID      string
	CapabilityVersion uint32
	// Purpose says what this read is for, in the plan's own words. It is not a hypothesis: the
	// opening plan is assembled before any hypothesis exists.
	Purpose string
}

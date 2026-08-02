package investigation

import "time"

// Controls is the fully resolved execution-control snapshot one round ran under.
//
// Every field is a RESTRICTION. ADR-015's invariant is that a customer-controlled policy may only
// restrict what OpenCluster may do and may never prescribe what it should inspect or in which
// order, so there is deliberately no field here naming a capability to prefer, a resource to look
// at first, or an order to work in. A field that told OpenCluster what it SHOULD do would be a
// planner change wearing a configuration option's clothes.
//
// The snapshot is pinned per round rather than read at display time, because "why did this round
// stop after two requests" has to stay answerable from the case alone, forever, without access to
// what the configuration says today.
type Controls struct {
	// MaxRequests is the most capability reads one round may make, across all its passes.
	MaxRequests int
	// MaxAdaptivePasses is how many times the planner may propose further reads from the
	// hypotheses it currently holds. Two in this slice, hard-capped.
	MaxAdaptivePasses int
	// MaxResultBytes is the total size of every result one round may accept.
	MaxResultBytes int64
	// Deadline is the wall clock one round may take.
	Deadline time.Duration
	// RequestTimeout bounds one dispatched read.
	RequestTimeout time.Duration
	// MaxMicroCents is the cost ceiling. Zero means no ceiling was configured, which is not the
	// same as a ceiling of zero — an operator fact, never a currency (ADR-015).
	MaxMicroCents int64
	// PermittedCapabilities is the intersection of what this build dispatches and what the
	// customer permits. Empty means everything this build dispatches, because a control that
	// composes by intersection with nothing configured restricts nothing.
	PermittedCapabilities []string
}

// DefaultControls is what a round runs under when a customer has authored nothing. They are the
// product's own defaults rather than a policy, and they are pinned into the round exactly as a
// customer's would be, so a case read next quarter does not have to know which it was.
//
// These are sized for an INVESTIGATION rather than for a demonstration. The first set was
// deliberately small so that the exhaustion path would be exercised, which was the right trade
// while nothing had ever called a model; it stopped being right the moment one did. A real
// incident spans more than one source — a workload, its events, its logs, and in time the
// integrations either side of it — and a round that may make eight reads across two passes cannot
// follow a chain of evidence far enough to be worth reading. A bound that stops an investigation
// before it has finished thinking does not save money; it spends it on an answer nobody can use.
//
// The exhaustion path is still tested, and better: a test tightens these through the composition
// root's wiring, so it asserts the same code against bounds it chooses rather than against bounds
// the product happens to ship. Nothing about the exhaustion behaviour depends on these numbers
// being small.
//
// Two of them are set by what a live provider actually does rather than by taste. A single
// reasoning call on a model that thinks before answering has been measured at over two minutes, so
// a round making several of them across several passes needs a wall clock in the tens of minutes;
// five would have failed a round that was working.
func DefaultControls() Controls {
	return Controls{
		// Across every pass. Enough to establish a workload, read what the cluster said, follow
		// two or three chains of evidence to their source, and still have reads left to falsify
		// the explanation that emerges.
		MaxRequests: 40,
		// Enough passes for a chain: orient, look, form a view, test it, and revise once when the
		// test says something unexpected. Two allows the first half of that and stops.
		MaxAdaptivePasses: 6,
		// The total size of every result one round may accept. What reaches the model is bounded
		// far below this when the prompt is rendered; this bounds what the round will hold.
		MaxResultBytes: 32 << 20,
		// The wall clock for the whole round, sized around reasoning calls that take minutes
		// rather than around reads that take seconds.
		Deadline: 45 * time.Minute,
		// One dispatched capability read. It bounds a source that has stopped answering, and it is
		// generous because a slow source is not a broken one.
		RequestTimeout: 3 * time.Minute,
	}
}

// Restrict composes two sets of controls by most-restrictive, uniformly and with no exceptions
// (ADR-015). Numeric limits compose by minimum, permitted capabilities by intersection. An
// unset numeric field on either side means "this side restricts nothing here" rather than zero,
// because a control nobody configured must not be the strictest one in the composition.
func (c Controls) Restrict(other Controls) Controls {
	return Controls{
		MaxRequests:           smallestPositive(c.MaxRequests, other.MaxRequests),
		MaxAdaptivePasses:     smallestPositive(c.MaxAdaptivePasses, other.MaxAdaptivePasses),
		MaxResultBytes:        smallestPositive(c.MaxResultBytes, other.MaxResultBytes),
		Deadline:              smallestPositive(c.Deadline, other.Deadline),
		RequestTimeout:        smallestPositive(c.RequestTimeout, other.RequestTimeout),
		MaxMicroCents:         smallestPositive(c.MaxMicroCents, other.MaxMicroCents),
		PermittedCapabilities: intersect(c.PermittedCapabilities, other.PermittedCapabilities),
	}
}

// Permits reports whether a capability is inside the intersection. An empty set permits
// everything this build dispatches, which is what an intersection with nothing configured means.
func (c Controls) Permits(capabilityID string) bool {
	if len(c.PermittedCapabilities) == 0 {
		return true
	}
	for _, permitted := range c.PermittedCapabilities {
		if permitted == capabilityID {
			return true
		}
	}
	return false
}

// Limit is which execution limit a round reached. Reaching one is NOT a failure: it terminates the
// reasoning and the round concludes or abstains on what it has, recording what it reached as a
// CoverageGap. A round that reached one before producing anything is the exception, and is a failed
// round — there is nothing to abstain from when the run never got far enough to have looked.
//
// The vocabulary is the glossary's: these are Execution limits, not budgets. CONTEXT.md bans the
// second word for this concept, because a budget is something you spend down and a limit is
// something you may not cross, and only one of those is what these are.
//
// The values are persisted and frozen by a gate in internal/gates.
type Limit int16

const (
	LimitRequests Limit = iota + 1
	LimitResultBytes
	LimitDeadline
	LimitCost
	LimitAdaptivePasses
)

func (l Limit) String() string {
	switch l {
	case LimitRequests:
		return "request count"
	case LimitResultBytes:
		return "result size"
	case LimitDeadline:
		return "wall clock"
	case LimitCost:
		return "cost ceiling"
	case LimitAdaptivePasses:
		return "adaptive passes"
	default:
		return "unrecognised"
	}
}

// smallestPositive composes one numeric restriction. Zero on either side means unset.
func smallestPositive[T int | int64 | time.Duration](a, b T) T {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// intersect composes permitted sets. An empty set on either side restricts nothing, so it is the
// other side that survives.
func intersect(a, b []string) []string {
	switch {
	case len(a) == 0:
		return b
	case len(b) == 0:
		return a
	}
	permitted := make(map[string]struct{}, len(b))
	for _, value := range b {
		permitted[value] = struct{}{}
	}
	both := make([]string, 0, min(len(a), len(b)))
	for _, value := range a {
		if _, ok := permitted[value]; ok {
			both = append(both, value)
		}
	}
	return both
}

package reasoning

import (
	"time"
)

// What one call consumed, and what is recorded about it.
//
// The distinction this file exists to keep is between a figure of zero and no figure at all. Zero
// is a measurement — the cache missed. Absent is the lack of one — the provider never said.
// Collapsing them makes a cache that stopped working indistinguishable from a provider that does
// not report caching, and the first is an incident while the second is a Tuesday.

// Count is a token figure and whether the provider actually reported it.
type Count struct {
	Tokens int64
	// Reported is false when the provider said nothing about this figure. A consumer that treats
	// an unreported count as zero is asserting a measurement nobody made.
	Reported bool
}

// Counted is a figure a provider reported.
func Counted(tokens int64) Count { return Count{Tokens: tokens, Reported: true} }

// Unreported is the absence of a figure.
func Unreported() Count { return Count{} }

// Or returns the figure, or the fallback when the provider reported none. It exists so the few
// places that genuinely must have a number say so at the call site.
func (c Count) Or(fallback int64) int64 {
	if !c.Reported {
		return fallback
	}
	return c.Tokens
}

// TokenUsage is one call's consumption, normalized across every provider.
type TokenUsage struct {
	Input      Count
	Output     Count
	CacheWrite Count
	CacheRead  Count
	// Reasoning is tokens spent on internal reasoning where the provider breaks them out. They
	// are already inside Output on every provider that reports both; this is a decomposition for
	// observability, never an addend.
	Reasoning Count
}

// Billable is the token total a round's spend is summed from. Cache reads and writes are input
// tokens the provider handled, so they count towards what was consumed even though they are
// priced differently.
func (u TokenUsage) Billable() int64 {
	return u.Input.Or(0) + u.Output.Or(0) + u.CacheWrite.Or(0) + u.CacheRead.Or(0)
}

// Record is everything worth knowing about one completed call, whatever answered it.
//
// It is one type rather than a scattering of log fields because the same set has to reach
// telemetry, an audit entry and a case file, and a set assembled separately in three places is a
// set that disagrees with itself within a quarter.
type Record struct {
	// Provider and RequestedModel are what configuration asked for.
	Provider       string
	RequestedModel string
	// AnsweringModel is what actually replied. It differs from RequestedModel when a provider
	// re-served the request itself, and it is the one written into the round's pinned versions.
	AnsweringModel string
	// RequestID is the provider's own identifier for the call.
	RequestID string
	Usage     TokenUsage
	// FellBack reports that an earlier configured deployment gave way to this one. A round that
	// fell back is not a failed round, but it is not an ordinary one either.
	FellBack bool
	// FellBackFrom names what gave way, so a regression after a provider change is attributable.
	FellBackFrom string
	Stop         Stop
	// MicroCents is what this call cost, in integers. Two calls that cost the same report the
	// same number, which is the one property a cost figure has to have.
	MicroCents int64
	Latency    time.Duration
	// Method names which of the boundary's three calls this was, so evidence selection stays
	// separable from the conclusion when the records are read back.
	Method        string
	PromptVersion string
	SchemaVersion string
}

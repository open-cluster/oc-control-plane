package agent

// What one call consumed.
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

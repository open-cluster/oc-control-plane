package reasoning

import "strings"

// THE CONTEXT BUDGET.
//
// A single-shot investigation cannot outgrow its own read budget, so it needs none. A
// conversation of ten follow-ups over a hundred tool results exceeds any model's window,
// and without a budget that turn simply fails.
//
// There is NO SINGLE UNIVERSAL CONSTANT here, because there is no single universal window:
// a deployment on a 200k model and one on a 1M model should not compact at the same point.
// The number is the configured window, or a per-model default, minus a reserve for the
// answer, times a soft threshold.
//
// Token counts are ESTIMATED FROM CHARACTERS, by the investigation domain, which is what
// measures its own context. A real tokenizer would be a dependency per vendor, kept in step
// with each one's releases, to compute a number that is compared against a threshold with a
// safety margin already built into it. The estimate is deliberately pessimistic, so it
// trims slightly early rather than overflowing — the two failure modes are not symmetric,
// and only one of them loses a turn.

// contextWindows maps a model-id substring to its window, in tokens. Matched as a
// substring, case-folded, so a dated snapshot or a vendor prefix resolves to the family it
// belongs to rather than falling through to the default.
//
// ORDER MATTERS: the first match wins, so a specific family must come before a prefix that
// would also match it.
var contextWindows = []struct {
	family string
	window int
}{
	{"claude-opus-4", 200_000},
	{"claude-sonnet-4", 200_000},
	{"claude-haiku-4", 200_000},
	{"claude", 200_000},
	{"glm-4.6", 200_000},
	{"glm-4.5", 128_000},
	{"glm", 128_000},
}

const (
	// defaultContextWindow is what an unrecognised model is assumed to have. Conservative
	// on purpose: being wrong low costs one early compaction, and being wrong high costs
	// the turn.
	defaultContextWindow = 128_000
	// responseHeadroom is kept back for the answer. A budget that filled the window would
	// leave no room for the thing the window was filled to produce.
	responseHeadroom = 16_000
)

// ContextWindow reports the working window for a model id, in tokens. An unrecognised model
// gets the conservative default rather than an error: a deployment configured with a model
// this build has never heard of should compact a little early, not refuse to investigate.
func ContextWindow(model string) int {
	folded := strings.ToLower(strings.TrimSpace(model))
	for _, known := range contextWindows {
		if strings.Contains(folded, known.family) {
			return known.window
		}
	}
	return defaultContextWindow
}

// ContextBudget is how much of a model's window a conversation may fill before older turns
// are compacted, in tokens.
//
// configured of zero means the per-model table decides. thresholdPercent is the soft
// threshold — compacting AT the ceiling would mean the very turn that triggered it has no
// room to run.
func ContextBudget(model string, configured, thresholdPercent int) int {
	window := configured
	if window <= 0 {
		window = ContextWindow(model)
	}
	usable := window - responseHeadroom
	if usable < responseHeadroom {
		// A window too small to reserve from is one where the reserve is the whole budget.
		// Refusing here would turn a misconfiguration into a deployment that cannot
		// investigate; compacting constantly is worse behavior and better than none.
		usable = responseHeadroom
	}
	if thresholdPercent <= 0 || thresholdPercent >= 100 {
		thresholdPercent = 50
	}
	return usable * thresholdPercent / 100
}

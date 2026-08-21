package reasoning

import "testing"

// The budget is model-specific because the windows are. A deployment on a 200k model and
// one on a 128k model must not compact at the same point, and a deployment that configured
// a number must get the number it configured.

func TestTheWindowIsResolvedPerModelWithAConservativeFallback(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		model string
		want  int
	}{
		{"claude-opus-4-20250514", 200_000},
		{"claude-sonnet-4-5", 200_000},
		{"CLAUDE-HAIKU-4-5", 200_000},
		{"glm-4.6", 200_000},
		{"glm-4.5-air", 128_000},
		// The reason the fallback is conservative rather than generous: being wrong low
		// costs one early compaction, and being wrong high costs the turn.
		{"a-model-this-build-has-never-heard-of", 128_000},
		{"", 128_000},
	} {
		if got := ContextWindow(testCase.model); got != testCase.want {
			t.Errorf("ContextWindow(%q) = %d, want %d", testCase.model, got,
				testCase.want)
		}
	}
}

// A configured window wins over the table, because a deployment that says what its model
// has knows better than a list compiled months ago.
func TestAConfiguredWindowOverridesTheTable(t *testing.T) {
	t.Parallel()

	budget := ContextBudget("claude-opus-4", 1_000_000, 50)
	if budget != (1_000_000-responseHeadroom)*50/100 {
		t.Errorf("budget = %d; a configured window must be what is used", budget)
	}
}

// The budget is a SOFT threshold below the usable window. Compacting at the ceiling would
// mean the very turn that triggered it has no room left to run.
func TestTheBudgetLeavesRoomForTheAnswerAndForTheTurn(t *testing.T) {
	t.Parallel()

	budget := ContextBudget("claude-opus-4", 0, 50)
	window := ContextWindow("claude-opus-4")

	if budget >= window-responseHeadroom {
		t.Errorf("budget %d leaves no soft margin below the usable window %d", budget,
			window-responseHeadroom)
	}
	if budget <= 0 {
		t.Fatalf("budget = %d", budget)
	}

	// A higher threshold buys more context and less headroom, which is the whole point of
	// making it configuration.
	if higher := ContextBudget("claude-opus-4", 0, 80); higher <= budget {
		t.Errorf("a threshold of 80%% gave %d, no more than 50%% gave %d", higher, budget)
	}
}

// A nonsense threshold falls back rather than producing a budget of zero — which would
// compact on every turn — or one at the ceiling, which would compact too late to help.
func TestAnUnusableThresholdFallsBack(t *testing.T) {
	t.Parallel()

	sensible := ContextBudget("claude-opus-4", 0, 50)
	for _, threshold := range []int{0, -1, 100, 1_000} {
		if got := ContextBudget("claude-opus-4", 0, threshold); got != sensible {
			t.Errorf("ContextBudget with threshold %d = %d, want the fallback %d",
				threshold, got, sensible)
		}
	}
}

// A window too small to reserve an answer from still yields a usable budget. Refusing here
// would turn one misconfigured number into a deployment that cannot investigate at all.
func TestAWindowTooSmallToReserveFromStillProducesABudget(t *testing.T) {
	t.Parallel()

	if budget := ContextBudget("anything", 1_000, 50); budget <= 0 {
		t.Errorf("budget = %d for a tiny configured window; it must stay usable", budget)
	}
}

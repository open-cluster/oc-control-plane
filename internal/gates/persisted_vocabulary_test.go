package gates_test

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Some persisted vocabularies are TEXT rather than integers: a finding's kind and
// confidence live inside a JSONB document, and the ceiling that stopped an investigation
// is a column an operator view and its clients key on. The integer gate beside this one
// cannot see them, and the compiler cannot either — renaming a constant's VALUE changes
// what every stored row means while every call site keeps compiling.
//
// So they are frozen here in the same way and for the same reason. A value that changes
// fails naming itself; a value that is ADDED fails too, which is the point: extending a
// persisted vocabulary is a decision, and this file is where it becomes visible.

// TestPersistedFindingVocabularyIsFrozen holds the words stored inside investigation
// findings. They travel to clients and are compared as strings by the operator surface.
func TestPersistedFindingVocabularyIsFrozen(t *testing.T) {
	t.Parallel()

	assertVocabulary(t, "investigation.FindingKinds", investigation.FindingKinds, []string{
		"probable_cause",
		"contributing_factor",
		"symptom",
		"triggering_change",
		"propagation_effect",
		"ruled_out",
		"unresolved_lead",
		// observation arrived with Conversations: a peacetime question establishes facts
		// with no causal role, and forcing one into "symptom" would be a lie about an
		// incident that is not happening.
		"observation",
	})

	assertVocabulary(t, "investigation.Confidences", investigation.Confidences, []string{
		"confirmed",
		"likely",
		"possible",
	})
}

// TestTheHonestStopsAreFrozen holds the ceilings stopped_by records. A client renders a
// stopped investigation differently from a freely concluded one, so a word that moved
// would silently start rendering resource exhaustion as a finished diagnosis.
func TestTheHonestStopsAreFrozen(t *testing.T) {
	t.Parallel()

	assertVocabulary(t, "the stopped_by vocabulary", []string{
		investigation.StoppedBySpend,
		investigation.StoppedByToolRuns,
		investigation.StoppedByReasonerTurns,
		investigation.StoppedByWallClock,
		investigation.StoppedByStagnation,
		investigation.StoppedByContext,
	}, []string{
		"spend",
		"tool_runs",
		"reasoner_turns",
		"wall_clock",
		"stagnation",
		// context arrived with Conversations: a turn whose transcript alone outgrows the
		// model's budget stops for a stated reason instead of failing.
		"context",
	})
}

// assertVocabulary compares a persisted word list against the words as they are stored,
// in order. Order matters as much as membership: several of these render as an ordered
// set to a client, and a list that quietly reordered is a list nobody can diff.
func assertVocabulary(t *testing.T, name string, got, frozen []string) {
	t.Helper()

	if len(got) != len(frozen) {
		t.Errorf("%s holds %d values and is frozen at %d (%s, against %s); a persisted "+
			"vocabulary is a storage contract, and adding to one is a decision that "+
			"belongs in this file", name, len(got), len(frozen),
			strings.Join(got, ", "), strings.Join(frozen, ", "))
		return
	}
	for index, wanted := range frozen {
		if got[index] != wanted {
			t.Errorf("%s[%d] is %q and is stored as %q; the value is persisted, so "+
				"changing it rewrites what every existing row means",
				name, index, got[index], wanted)
		}
	}
}

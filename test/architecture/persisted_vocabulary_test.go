package gates_test

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestFindingVocabulariesSeparateHistoricalReadsFromNewOutput(t *testing.T) {
	t.Parallel()

	assertVocabulary(t, "investigation.FindingKinds", investigation.FindingKinds, []string{
		"cause",
		"trigger",
		"contributing_factor",
		"symptom",
		"propagation",
		"ruled_out",
		"unresolved",
		"observation",
	})
	assertVocabulary(t, "investigation.GeneratedFindingKinds", investigation.GeneratedFindingKinds, []string{
		"cause",
		"contributing_factor",
		"observation",
		"ruled_out",
	})

	assertVocabulary(t, "investigation.ConclusionStatuses", investigation.ConclusionStatuses,
		[]string{"verified_cause", "supported_explanation", "inconclusive", "answer_only"})
	assertVocabulary(t, "investigation.HypothesisStatuses", investigation.HypothesisStatuses,
		[]string{"exploring", "supported", "ruled_out", "unresolved"})
	assertVocabulary(t, "investigation.LimitationTypes", investigation.LimitationTypes,
		[]string{"missing_telemetry", "missing_access", "contradiction",
			"unresolved_assumption", "essential_human_input"})
}

func TestTheHonestStopsAreFrozen(t *testing.T) {
	t.Parallel()

	assertVocabulary(t, "the stopped_by vocabulary", []string{
		investigation.StoppedByToolRuns,
		investigation.StoppedByReasonerTurns,
		investigation.StoppedByWallClock,
		investigation.StoppedByStagnation,
		investigation.StoppedByContext,
	}, []string{
		"tool_runs",
		"reasoner_turns",
		"wall_clock",
		"stagnation",
		"context",
	})
}

func TestSlackMessageRecoveryAuditActionIsFrozen(t *testing.T) {
	t.Parallel()

	if audit.ActionSlackMessageRecovered != "slack-message.recovered" {
		t.Errorf("ActionSlackMessageRecovered = %q, want slack-message.recovered",
			audit.ActionSlackMessageRecovered)
	}
}

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

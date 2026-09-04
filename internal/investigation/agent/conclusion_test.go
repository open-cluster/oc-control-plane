package agent

import (
	"encoding/json"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/test/eval"
	"strings"
	"testing"
)

func TestStructuredConclusionContractRequiresMechanismForAVerifiedCause(t *testing.T) {
	t.Parallel()

	properties, ok := ConcludeDefinition().InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the conclude schema has no properties")
	}
	for _, field := range []string{
		"status", "summary", "impact", "findings", "hypotheses", "actions", "limitations",
	} {
		if _, present := properties[field]; !present {
			t.Errorf("the conclude schema does not offer %q", field)
		}
	}

	document, err := json.Marshal(map[string]any{
		"status":  "verified_cause",
		"summary": "The deployment caused the checkout outage.",
		"impact": map[string]any{
			"status": "known", "current_state": "ongoing",
			"affected_services": []string{"checkout-api"}, "affected_users": []string{},
			"summary": "Checkout requests are failing.", "run_refs": []int{1},
		},
		"findings": []map[string]any{{
			"id": "finding-1", "statement": "Deployment abc123 caused the outage.",
			"kind": "cause", "confidence": "confirmed", "mechanism": "", "run_refs": []int{1},
		}},
		"hypotheses":  []map[string]any{},
		"actions":     []map[string]any{},
		"limitations": []map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := decodeConclusion(document, 1, false); err == nil {
		t.Fatal("a verified cause without a causal mechanism was accepted")
	}

	_ = investigation.VerifiedCause
}

func TestStructuredConclusionEvaluationFixtures(t *testing.T) {
	t.Parallel()

	_, fixtures, err := eval.LoadStructuredResults()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			encoded, marshalErr := json.Marshal(fixture.Document)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			_, decodeErr := decodeConclusion(encoded, fixture.Runs, false)
			if fixture.Valid && decodeErr != nil {
				t.Fatalf("valid fixture was rejected: %v", decodeErr)
			}
			if !fixture.Valid && (decodeErr == nil || !strings.Contains(decodeErr.Error(), fixture.ErrorContains)) {
				t.Fatalf("error = %v, want %q", decodeErr, fixture.ErrorContains)
			}
		})
	}
}

func TestStructuredConclusionRequiresCitationsForImpactAndActions(t *testing.T) {
	t.Parallel()

	document := map[string]any{
		"status": "inconclusive", "summary": "The cause is not established.",
		"impact": map[string]any{
			"status": "partial", "current_state": "ongoing",
			"affected_services": []string{"checkout-api"}, "affected_users": []string{},
			"summary": "Checkout is degraded.", "run_refs": []int{},
		},
		"findings": []map[string]any{}, "hypotheses": []map[string]any{},
		"actions": []map[string]any{}, "limitations": []map[string]any{},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeConclusion(encoded, 1, false); err == nil {
		t.Fatal("partial impact without a Run reference was accepted")
	}

	document["impact"] = map[string]any{
		"status": "unknown", "current_state": "unknown",
		"affected_services": []string{}, "affected_users": []string{},
		"summary": "Impact is unknown.", "run_refs": []int{},
	}
	document["actions"] = []map[string]any{{
		"title": "Monitor recovery", "type": "monitor", "rationale": "Confirm recovery.",
		"risk": "low", "reversible": true, "requires_approval": false,
		"verification": "Latency returns to baseline.", "run_refs": []int{},
	}}
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeConclusion(encoded, 1, false); err == nil {
		t.Fatal("an action without a Run reference was accepted")
	}
}

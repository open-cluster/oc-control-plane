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
	assertSchemaFields(t, properties["impact"], "run_refs", "summary")
	findings := properties["findings"].(map[string]any)
	assertSchemaFields(t, findings["items"], "evidence_refs", "kind", "mechanism", "run_refs", "statement")
	actions := properties["actions"].(map[string]any)
	assertSchemaFields(t, actions["items"], "rationale", "run_refs", "title", "verification")
	hypotheses := properties["hypotheses"].(map[string]any)
	assertSchemaFields(t, hypotheses["items"], "run_refs", "statement", "status", "test")
	if SchemaVersion != "9" {
		t.Errorf("conclusion schema revision = %q, want 9", SchemaVersion)
	}

	document, err := json.Marshal(map[string]any{
		"status":  "verified_cause",
		"summary": "The deployment caused the checkout outage.",
		"impact": map[string]any{
			"summary": "Checkout requests are failing.", "run_refs": []int{1},
		},
		"findings": []map[string]any{{
			"statement": "Deployment abc123 caused the outage.",
			"kind":      "cause", "mechanism": "", "run_refs": []int{1}, "evidence_refs": []any{},
		}},
		"hypotheses":  []map[string]any{},
		"actions":     []map[string]any{},
		"limitations": []map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := decodeConclusion(document, 1, nil); err == nil {
		t.Fatal("a verified cause without a causal mechanism was accepted")
	}

	_ = investigation.VerifiedCause
}

func assertSchemaFields(t *testing.T, candidate any, want ...string) {
	t.Helper()
	schema, ok := candidate.(map[string]any)
	if !ok {
		t.Fatalf("schema = %#v", candidate)
	}
	fields, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	if len(fields) != len(want) {
		t.Fatalf("schema fields = %v, want %v", fields, want)
	}
	for _, name := range want {
		if _, present := fields[name]; !present {
			t.Errorf("schema omits %q", name)
		}
	}
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
			_, decodeErr := decodeConclusion(encoded, fixture.Runs, nil)
			if fixture.Valid && decodeErr != nil {
				t.Fatalf("valid fixture was rejected: %v", decodeErr)
			}
			if !fixture.Valid && (decodeErr == nil || !strings.Contains(decodeErr.Error(), fixture.ErrorContains)) {
				t.Fatalf("error = %v, want %q", decodeErr, fixture.ErrorContains)
			}
		})
	}
}

func TestStructuredConclusionRejectsRemovedFindingKinds(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"trigger", "symptom", "propagation", "unresolved"} {
		t.Run(kind, func(t *testing.T) {
			document := map[string]any{
				"status": "answer_only", "summary": "A deployment preceded the alert.",
				"impact": map[string]any{"summary": "Impact is not established.", "run_refs": []int{}},
				"findings": []map[string]any{{
					"statement": "Deployment abc123 preceded the alert.", "kind": kind,
					"mechanism": "", "run_refs": []int{1}, "evidence_refs": []any{},
				}},
				"hypotheses": []any{}, "actions": []any{}, "limitations": []any{},
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = decodeConclusion(encoded, 1, nil); err == nil ||
				!strings.Contains(err.Error(), "declared vocabulary") {
				t.Fatalf("removed finding kind %q was accepted: %v", kind, err)
			}
		})
	}
}

func TestStructuredConclusionRequiresCitationsForActions(t *testing.T) {
	t.Parallel()

	document := map[string]any{
		"status": "inconclusive", "summary": "The cause is not established.",
		"impact":   map[string]any{"summary": "Impact is not established.", "run_refs": []int{}},
		"findings": []map[string]any{}, "hypotheses": []map[string]any{},
		"actions": []map[string]any{{
			"title": "Monitor recovery", "rationale": "Confirm recovery.",
			"verification": "Latency returns to baseline.", "run_refs": []int{},
		}},
		"limitations": []map[string]any{},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeConclusion(encoded, 1, nil); err == nil {
		t.Fatal("an action without a Run reference was accepted")
	}
}

func TestStructuredConclusionRejectsRemovedHypothesisIdentity(t *testing.T) {
	t.Parallel()

	document := map[string]any{
		"status": "inconclusive", "summary": "The cause is not established.",
		"impact":   map[string]any{"summary": "Impact is not established.", "run_refs": []int{}},
		"findings": []map[string]any{},
		"hypotheses": []map[string]any{{
			"id": "legacy-hypothesis", "statement": "A deployment may be involved.",
			"status": "unresolved", "test": "Inspect deployment history.", "run_refs": []int{},
		}},
		"actions": []map[string]any{}, "limitations": []map[string]any{},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeConclusion(encoded, 0, nil); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("removed hypothesis identity was accepted: %v", err)
	}
}

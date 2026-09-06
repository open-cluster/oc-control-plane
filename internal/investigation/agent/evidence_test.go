package agent

import (
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"strings"
	"testing"
)

func TestConclusionPreservesCrossInvestigationEvidence(t *testing.T) {
	document := []byte(`{
		"status":"answer_only", "summary":"Earlier observations remain available.",
		"impact":{"status":"unknown","current_state":"unknown","summary":"Impact is unknown.","run_refs":[]},
		"findings":[{"id":"prior","statement":"The earlier deploy changed the pool.",
		"kind":"observation","confidence":"confirmed","mechanism":"","run_refs":[],
		"evidence_refs":[{"investigationId":"11111111-1111-4111-8111-111111111111","toolRunOrdinal":1}]}]
	}`)
	ref := investigation.EvidenceRef{InvestigationID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), ToolRunOrdinal: 1}
	conclusion, err := decodeConclusion(document, 0, []investigation.EvidenceRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(conclusion.Findings) != 1 || len(conclusion.Findings[0].EvidenceRefs) != 1 || conclusion.Findings[0].EvidenceRefs[0] != ref {
		t.Fatalf("lost reused Finding: %+v", conclusion)
	}
	if _, err := decodeConclusion(document, 0, nil); err == nil {
		t.Fatal("accepted a citation outside the authorized prior evidence")
	}
	pair := `{"investigationId":"11111111-1111-4111-8111-111111111111","toolRunOrdinal":1}`
	duplicated := []byte(strings.Replace(string(document), pair, pair+","+pair, 1))
	if _, err := decodeConclusion(duplicated, 0, []investigation.EvidenceRef{ref}); err == nil {
		t.Fatal("accepted duplicate evidence references")
	}
}

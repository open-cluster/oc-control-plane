package investigation

import (
	"encoding/json"
	"testing"
)

func TestHistoricalConclusionRetainsContentAndIgnoresDroppedFields(t *testing.T) {
	t.Parallel()

	stored := []byte(`{
		"status":"answer_only","summary":"A deployment preceded the alert.",
		"impact":{"status":"partial","currentState":"recovering","affectedServices":["checkout-api"],"summary":"Checkout was degraded.","runRefs":[1]},
		"findings":[{"id":"f1","statement":"Deployment abc123 preceded the alert.","kind":"trigger","confidence":"likely","mechanism":"","runRefs":[1]}],
		"hypotheses":[],
		"actions":[{"title":"Verify recovery","type":"verify","rationale":"Confirm the service recovered.","risk":"low","reversible":true,"requiresApproval":false,"verification":"Error rate is at baseline.","runRefs":[1]}],
		"limitations":[]
	}`)
	var conclusion Conclusion
	if err := json.Unmarshal(stored, &conclusion); err != nil {
		t.Fatal(err)
	}
	if conclusion.Summary != "A deployment preceded the alert." ||
		conclusion.Impact.Summary != "Checkout was degraded." ||
		len(conclusion.Findings) != 1 || conclusion.Findings[0].Kind != FindingTrigger ||
		len(conclusion.Actions) != 1 || conclusion.Actions[0].Verification != "Error rate is at baseline." {
		t.Fatalf("historical content was not retained: %+v", conclusion)
	}
}

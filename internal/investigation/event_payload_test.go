package investigation

import (
	"encoding/json"
	"testing"
)

func TestToolStartedEventCarriesProgressWithoutDuplicatingArguments(t *testing.T) {
	payload := ToolStartedPayload(ToolRun{
		Ordinal: 3, Tool: "github.read_commits", Purpose: "check the change window",
		Arguments: map[string]any{"repositoryId": float64(42)},
	}, "integration-1", "Production GitHub")

	document, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var rendered map[string]any
	if err := json.Unmarshal(document, &rendered); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"arguments", "hypothesisId"} {
		if _, present := rendered[removed]; present {
			t.Errorf("tool_started duplicates %s: %s", removed, document)
		}
	}
	if rendered["tool"] != "github.read_commits" ||
		rendered["purpose"] != "check the change window" {
		t.Errorf("progress metadata was lost: %s", document)
	}
}

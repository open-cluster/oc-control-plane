package investigation

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"
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

func TestInvestigationEventsUseTheSevenSmallPayloadContracts(t *testing.T) {
	if EventSchemaVersion != 2 {
		t.Fatalf("event schema version = %d, want 2", EventSchemaVersion)
	}

	cases := []struct {
		name    string
		payload EventPayload
		fields  []string
	}{
		{name: "started", payload: StartedPayload(Investigation{Subject: "must not leak"}, true), fields: []string{}},
		{name: "progress", payload: ProgressPayload("reading evidence"), fields: []string{"text"}},
		{name: "tool started", payload: ToolStartedPayload(ToolRun{Ordinal: 1, Tool: "github.read", Purpose: "read commits"}, "00000000-0000-0000-0000-000000000001", "GitHub"), fields: []string{"integration", "integrationId", "ordinal", "purpose", "tool"}},
		{name: "tool completed", payload: ToolCompletedPayload(ToolRun{Ordinal: 1, Outcome: RunFailed, Summary: "   ", Error: "provider token sk-secret-value", StartedAt: time.Unix(0, 0), FinishedAt: time.Unix(0, int64(time.Second))}), fields: []string{"durationMs", "ordinal", "outcome", "summary", "truncated"}},
		{name: "concluded", payload: ConcludedPayload(Conclusion{Status: Inconclusive, Summary: "answer"}, "must not leak"), fields: []string{"status", "summary"}},
		{name: "failed", payload: FailedPayload("safe reason"), fields: []string{"reason"}},
		{name: "cancelled", payload: CancelledPayload(), fields: []string{"message"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(document, &object); err != nil {
				t.Fatal(err)
			}
			fields := make([]string, 0, len(object))
			for field := range object {
				fields = append(fields, field)
			}
			slices.Sort(fields)
			if !reflect.DeepEqual(fields, test.fields) {
				t.Fatalf("fields = %v, want %v: %s", fields, test.fields, document)
			}
			if test.name == "tool completed" && object["summary"] != "Tool failed" {
				t.Errorf("failed completion summary = %q, want generic safe summary", object["summary"])
			}
			if test.name == "tool completed" && object["truncated"] != false {
				t.Errorf("truncated:false was omitted: %s", document)
			}
		})
	}
}

func TestToolCompletedUsesOnlyKnownSafeFailureDescriptions(t *testing.T) {
	tests := []struct {
		name string
		run  ToolRun
		want string
	}{
		{
			name: "unavailable tool",
			run: ToolRun{Outcome: RunFailed,
				Error: "not one of the tools the selected sources offer"},
			want: "not one of the tools the selected sources offer",
		},
		{
			name: "provider error with a spoofed internal prefix",
			run: ToolRun{Outcome: RunFailed,
				Error: "not executed: provider rejected token sk-secret-value"},
			want: "Tool failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ToolCompletedPayload(test.run).Summary; got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

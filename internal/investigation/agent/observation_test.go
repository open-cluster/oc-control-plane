package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestRunBoundsPreparedObservations(t *testing.T) {
	for _, example := range []struct {
		name                   string
		content                any
		arguments              map[string]any
		sourceLimited, clipped bool
	}{
		{"UTF-8 and escaping", strings.Repeat("界\"\n<>&", 2000), nil, false, true},
		{"list items", []string{strings.Repeat("界", 4096), strings.Repeat("界", 4096)}, nil, false, true},
		{"list boundary", []string{"a", strings.Repeat("b", 16377)}, nil, false, true},
		{"oversized arguments", "value", map[string]any{"query": strings.Repeat("x", 70<<10)}, false, true},
		{"source limit", "sample", nil, true, false},
	} {
		t.Run(example.name, func(t *testing.T) {
			store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
			catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				return integrations.ToolResult{Content: example.content, Truncated: example.sourceLimited}, nil
			})
			model := &scriptedModel{next: func(step int, prompt Prompt) (Completion, error) {
				if step == 1 {
					input := example.arguments
					if input == nil {
						input = map[string]any{}
					}
					arguments, _ := json.Marshal(map[string]any{"input": input})
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "read", Name: "stub_read", Arguments: arguments}}}, nil
				}
				run, feedback := store.runs[0], prompt.Turns[0].ToolResults[0]
				if run.ObservationText == nil || *run.ObservationText != feedback.Content || feedback.CallID != "read" || feedback.IsError ||
					len(feedback.Content) > investigation.MaxObservationBytes || !utf8.ValidString(feedback.Content) ||
					run.ModelClipped != example.clipped || run.Truncated != example.sourceLimited || run.Content != nil {
					t.Fatalf("prepared observation lost bounds, identity or clipping: bytes=%d modelClipped=%t sourceLimited=%t", len(feedback.Content), run.ModelClipped, run.Truncated)
				}
				if example.name == "oversized arguments" && (len(run.Arguments) != 0 || !strings.Contains(feedback.Content, "omitted")) {
					t.Fatal("oversized arguments were retained without explicit omission feedback")
				}
				if example.name == "list boundary" && !strings.Contains(feedback.Content, "[1 of 2 items") {
					t.Fatal("list clipping failed to account for the comma and closing bracket")
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, catalog)
			if err := agent.Run(t.Context(), uuid.New(), investigation.Investigation{ID: uuid.New(), Subject: "bounded observation"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunRecordsRefusedAndDuplicateCallsWithoutRepeatingARead(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
	executed := 0
	catalog := testCatalog(t, func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		content := "value"
		if request.Arguments["key"] == "b" {
			content = strings.Repeat("v", 17<<10)
		}
		at := time.Unix(1700000000, 0).UTC()
		return integrations.ToolResult{Summary: "value", Content: content,
			Truncated: request.Arguments["key"] == "a", WindowFrom: at, WindowUntil: at}, nil
	})
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{
				{ID: "invalid-input", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read it","input":[]}`)},
				{ID: "read", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read it","input":{"key":"a"}}`)},
				{ID: "duplicate", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read it","input":{"key":"a"}}`)},
				{ID: "unknown", Name: "invented.read", Arguments: json.RawMessage(`{"purpose":"guess","input":{}}`)},
				{ID: "invalid-json", Name: "stub_read", Arguments: json.RawMessage(`{"input":`)},
				{ID: "without-purpose", Name: "stub_read", Arguments: json.RawMessage(`{"input":{"key":"b"}}`)},
			}}, nil
		}
		if call == 2 {
			if len(prompt.Turns[0].ToolResults) != 6 || prompt.Turns[0].ToolResults[0].CallID != "invalid-input" ||
				!prompt.Turns[0].ToolResults[0].IsError || prompt.Turns[0].ToolResults[4].CallID != "invalid-json" ||
				!prompt.Turns[0].ToolResults[4].IsError {
				t.Fatal("invalid Tool input lost its correlated rejection feedback")
			}
			for index, id := range []string{"invalid-input", "read", "duplicate", "unknown", "invalid-json", "without-purpose"} {
				if prompt.Turns[0].ToolResults[index].CallID != id {
					t.Fatal("Tool feedback lost call ordering or identity")
				}
			}
			limited, clipped := prompt.Turns[0].ToolResults[1], prompt.Turns[0].ToolResults[5]
			if limited.IsError || !strings.Contains(limited.Content, "may be incomplete") ||
				strings.Contains(limited.Content, "the source held more") ||
				!strings.Contains(limited.Content, "WINDOW: effective bounds") || strings.Contains(limited.Content, "[cut at") {
				t.Fatalf("source limits or instant evaluation bounds overstated: %s", limited.Content)
			}
			if clipped.IsError || !strings.Contains(clipped.Content, "[cut at 16384 bytes]") ||
				strings.Contains(clipped.Content, "may be incomplete") || store.runs[5].Truncated {
				t.Fatal("model clipping was confused with source incompleteness")
			}
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "refresh", Name: "stub_read", Arguments: json.RawMessage(`{"input":{"key":"a"}}`),
			}}}, nil
		}
		if len(store.runs) != 7 || store.runs[0].IntegrationID != store.candidate.ID {
			t.Fatal("rejected input lost its Tool Run or Integration")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if executed != 3 || len(store.runs) != 7 || store.status != investigation.StatusConcluded {
		t.Fatalf("executed=%d runs=%d status=%s", executed, len(store.runs), store.status)
	}
	for _, index := range []int{0, 2, 3, 4} {
		if store.runs[index].Outcome != investigation.RunFailed {
			t.Errorf("run %d outcome=%v, want failed evidence", index+1, store.runs[index].Outcome)
		}
	}
	if store.runs[5].Outcome != investigation.RunSucceeded || store.runs[5].Purpose != "" {
		t.Fatal("an authorized read without purpose must succeed without invented intent")
	}
}

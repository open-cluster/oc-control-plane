package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestRunEmitsDocumentedToolStartedProperties(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "Production source"}}
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "read", Name: "stub.read",
				Arguments: json.RawMessage(`{"purpose":"inspect source","input":{}}`)}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
			Arguments: validConclusion(t, []int{1})}}}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "available"}, nil
	}))
	org, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), Subject: "source"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range store.events {
		if event.Type == investigation.EventToolStarted {
			if event.Payload["integration"] != "Production source" {
				t.Fatal("Tool-started event lost its Integration display name")
			}
			return
		}
	}
	t.Fatal("no Tool-started event emitted")
}

func TestRunEmitsDocumentedCanonicalAnswerLength(t *testing.T) {
	for _, character := range []string{"a", "界"} {
		t.Run(character, func(t *testing.T) {
			answer := strings.Repeat(character, 4096)
			store := &records{}
			var document map[string]any
			if err := json.Unmarshal(validConclusion(t, nil), &document); err != nil {
				t.Fatal(err)
			}
			document["summary"] = answer
			arguments, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
					Arguments: arguments}}}, nil
			}}
			runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				return integrations.ToolResult{}, nil
			}))
			org, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
			if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
				t.Fatal(err)
			}
			if store.conclusion.Summary != answer {
				t.Fatal("canonical answer changed within its accepted length")
			}
			for _, event := range store.events {
				if event.Type == investigation.EventConcluded {
					if event.Payload["summary"] != store.conclusion.Summary {
						t.Fatal("concluded event did not retain the canonical answer")
					}
					return
				}
			}
			t.Fatal("no concluded event emitted")
		})
	}
}

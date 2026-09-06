package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"gopkg.in/yaml.v3"
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
	org, _ := tenancy.NewOrganization("org-test")
	if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), Subject: "source"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range store.events {
		if event.Type == investigation.EventToolStarted {
			if event.Payload["integration"] != "Production source" {
				t.Fatal("Tool-started event lost its Integration display name")
			}
			assertEventPayloadProperties(t, "ToolStartedInvestigationEvent", event.Payload)
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
			org, _ := tenancy.NewOrganization("org-test")
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
					assertEventPayloadProperties(t, "ConcludedInvestigationEvent", event.Payload)
					return
				}
			}
			t.Fatal("no concluded event emitted")
		})
	}
}

func assertEventPayloadProperties(t *testing.T, name string, payload map[string]any) {
	t.Helper()
	contents, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					AdditionalProperties any      `yaml:"additionalProperties"`
					Required             []string `yaml:"required"`
					Properties           map[string]struct {
						Type      string `yaml:"type"`
						MinLength int    `yaml:"minLength"`
						MaxLength int    `yaml:"maxLength"`
					} `yaml:"properties"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	schema := document.Components.Schemas[name].Properties["payload"]
	if schema.AdditionalProperties != false || len(schema.Properties) == 0 {
		t.Fatal("expected a declared closed payload schema")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var serialized map[string]any
	if err := json.Unmarshal(encoded, &serialized); err != nil {
		t.Fatal(err)
	}
	for _, required := range schema.Required {
		if _, present := serialized[required]; !present {
			t.Errorf("%s omitted required %s", name, required)
		}
	}
	for field, value := range serialized {
		property, present := schema.Properties[field]
		if !present {
			t.Errorf("%s emitted undeclared property %s", name, field)
			continue
		}
		if property.Type == "string" {
			text, ok := value.(string)
			length := utf8.RuneCountInString(text)
			if !ok || length < property.MinLength || (property.MaxLength > 0 && length > property.MaxLength) {
				t.Errorf("%s.%s violates its declared string bound: length=%d", name, field, length)
			}
		}
	}
}

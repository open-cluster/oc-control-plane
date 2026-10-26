package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

func TestRunSelectsGitHubIntegrationThroughTheToolEnvelope(t *testing.T) {
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	var candidates []integrations.Integration
	for _, name := range []string{"Production GitHub", "Staging GitHub", "Unverified scope"} {
		candidate := integrations.Integration{ID: uuid.New(), Provider: "github", Name: name,
			Configuration: map[string]any{"repository": name}, Status: integrations.StatusVerified,
			VerifiedAt: time.Now(), VerificationGrants: []string{"commits:read"},
		}
		candidate.CredentialSealed, err = sealer.Seal("credential for "+name, integrations.CredentialBinding(candidate.ID))
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	candidates[2].VerificationGrants = nil
	var reads []uuid.UUID
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{Key: "github", Name: "GitHub", Category: integrations.CategorySourceControl,
			Tools: []integrations.Tool{{Name: "github_read_commits", Description: "Read repository commits.",
				RequiredGrants: []string{"commits:read"},
				Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
					if request.Credential != "credential for "+request.Integration.Name ||
						request.Integration.Configuration["repository"] != request.Integration.Name {
						t.Fatal("source selection changed the credential or scope")
					}
					if _, routed := request.Arguments["integrationId"]; routed {
						t.Fatal("routing metadata leaked into provider operation arguments")
					}
					reads = append(reads, request.Integration.ID)
					return integrations.ToolResult{Summary: "commits read"}, nil
				}}},
		},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &records{}
	model := &scriptedModel{next: func(round int, prompt Prompt) (Completion, error) {
		if round == 1 {
			var offered *integrations.ToolDefinition
			for index := range prompt.Tools {
				if prompt.Tools[index].Name == "github_read_commits" {
					if offered != nil {
						t.Fatal("the operation was offered more than once")
					}
					offered = &prompt.Tools[index]
				}
			}
			if offered == nil {
				t.Fatal("the canonical operation was replaced with qualified names")
			}
			properties := offered.InputSchema["properties"].(map[string]any)
			selection, ok := properties["integrationId"].(map[string]any)
			if !ok || len(selection["enum"].([]any)) != 2 {
				t.Fatalf("eligible source selection schema = %v", properties)
			}
			required, _ := json.Marshal(offered.InputSchema["required"])
			if !strings.Contains(string(required), "integrationId") {
				t.Fatal("ambiguous source selection was not required")
			}
			encoded, _ := json.Marshal(selection)
			for _, candidate := range candidates[:2] {
				if !strings.Contains(string(encoded), candidate.ID.String()) || !strings.Contains(string(encoded), candidate.Name) {
					t.Fatal("the model cannot distinguish the eligible sources")
				}
			}
			if strings.Contains(string(encoded), candidates[2].ID.String()) {
				t.Fatal("a source without the required grant was offered")
			}
			var calls []CompletionCall
			for index, candidate := range []integrations.Integration{candidates[0], candidates[1], candidates[0]} {
				arguments, _ := json.Marshal(map[string]any{"integrationId": candidate.ID.String(), "input": map[string]any{}})
				calls = append(calls, CompletionCall{ID: []string{"production", "staging", "duplicate"}[index],
					Name: "github_read_commits", Arguments: arguments})
			}
			return Completion{Stop: StopToolUse, ToolCalls: calls}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
			Arguments: validConclusion(t, []int{1, 2})}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	agent.Store = &candidateRecords{records: store, candidates: candidates}
	agent.Sealer = sealer
	if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{ID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0] != candidates[0].ID || reads[1] != candidates[1].ID || store.unseals != 2 {
		t.Fatalf("executed sources=%v credential audits=%d", reads, store.unseals)
	}
	if len(store.runs) != 3 || store.runs[0].IntegrationID != reads[0] || store.runs[1].IntegrationID != reads[1] ||
		store.runs[2].IntegrationID != reads[0] || store.runs[2].Outcome != investigation.RunFailed {
		t.Fatalf("persisted source provenance = %+v", store.runs)
	}
}

func TestRunRejectsInvalidSourcesAndAutomaticallyBindsASingleSource(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run([]string{"single source", "multiple sources"}[count-1], func(t *testing.T) {
			candidates := []integrations.Integration{
				{ID: uuid.New(), Provider: "stub", Name: "Production", Status: integrations.StatusVerified, VerifiedAt: time.Now()},
				{ID: uuid.New(), Provider: "stub", Name: "Staging", Status: integrations.StatusVerified, VerifiedAt: time.Now()},
			}
			var reads []uuid.UUID
			catalog := testCatalog(t, func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
				reads = append(reads, request.Integration.ID)
				return integrations.ToolResult{Summary: "read succeeded"}, nil
			})
			store := &records{}
			model := &scriptedModel{next: func(round int, prompt Prompt) (Completion, error) {
				if round != 1 {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
						Arguments: validConclusion(t, nil)}}}, nil
				}
				var properties map[string]any
				for _, definition := range prompt.Tools {
					if definition.Name == "stub_read" {
						properties = definition.InputSchema["properties"].(map[string]any)
					}
				}
				_, selectionOffered := properties["integrationId"]
				if selectionOffered != (count > 1) {
					t.Fatal("routing metadata was offered without source ambiguity")
				}
				var calls []CompletionCall
				for index, id := range []any{nil, candidates[0].ID.String(), uuid.New().String(), 7, "", "invalid"} {
					arguments := map[string]any{"input": map[string]any{}}
					if index != 0 {
						arguments["integrationId"] = id
					}
					encoded, _ := json.Marshal(arguments)
					calls = append(calls, CompletionCall{ID: string(rune('a' + index)), Name: "stub_read", Arguments: encoded})
				}
				for index, name := range []string{"stub.read", "stub_read__" + candidates[0].ID.String()} {
					calls = append(calls, CompletionCall{ID: []string{"dotted", "qualified"}[index], Name: name,
						Arguments: json.RawMessage(`{"input":{}}`)})
				}
				return Completion{Stop: StopToolUse, ToolCalls: calls}, nil
			}}
			agent := configuredTestAgent(t, store, model, catalog)
			agent.Store = &candidateRecords{records: store, candidates: candidates[:count]}
			if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{ID: uuid.New()}); err != nil {
				t.Fatal(err)
			}
			if len(reads) != 1 || reads[0] != candidates[0].ID || len(store.runs) != 8 {
				t.Fatalf("executed sources=%v attempts=%d", reads, len(store.runs))
			}
			succeeded := 0
			for _, run := range store.runs {
				if run.Outcome == investigation.RunSucceeded {
					succeeded++
				}
				if _, leaked := run.Arguments["integrationId"]; leaked {
					t.Fatal("routing metadata became operation input")
				}
			}
			if succeeded != 1 {
				t.Fatalf("%d invalid or duplicate requests established evidence", succeeded)
			}
		})
	}
}

func TestRunKeepsSelectedIntegrationWhenTheReadBudgetIsExhausted(t *testing.T) {
	candidates := []integrations.Integration{
		{ID: uuid.New(), Provider: "stub", Name: "Production", Status: integrations.StatusVerified, VerifiedAt: time.Now()},
		{ID: uuid.New(), Provider: "stub", Name: "Staging", Status: integrations.StatusVerified, VerifiedAt: time.Now()},
	}
	store := &records{}
	model := &scriptedModel{next: func(round int, _ Prompt) (Completion, error) {
		if round != 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
				Arguments: validConclusion(t, []int{1})}}}, nil
		}
		var calls []CompletionCall
		for index, candidate := range candidates {
			arguments, _ := json.Marshal(map[string]any{"integrationId": candidate.ID.String(), "input": map[string]any{}})
			calls = append(calls, CompletionCall{ID: []string{"production", "staging"}[index], Name: "stub_read", Arguments: arguments})
		}
		return Completion{Stop: StopToolUse, ToolCalls: calls}, nil
	}}
	reads := 0
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			reads++
			return integrations.ToolResult{Summary: "read succeeded"}, nil
		}))
	agent.Store = &candidateRecords{records: store, candidates: candidates}
	agent.MaxToolRuns = 1
	if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{ID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(store.runs) != 2 || store.runs[1].IntegrationID != candidates[1].ID ||
		store.runs[1].Outcome != investigation.RunFailed {
		t.Fatalf("budget-refused source lost provenance: reads=%d runs=%+v", reads, store.runs)
	}
}

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
)

func TestRunContinuesAfterContextRecoveryWithHistoryAndToolEvidence(t *testing.T) {
	prior := uuid.New()
	store := &records{
		candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "Production"},
		messages:  []investigation.AssignedMessage{{Sequence: 7, Author: "alice", Text: "Check the current deployment."}},
		brief: investigation.Brief{HistoryBefore: 7, NextBefore: 3, MissingEvidence: true,
			Recent:         []investigation.BriefMessage{{Text: "OPTIONAL EXCHANGE"}},
			Findings:       []investigation.PriorFinding{{InvestigationID: prior, Statement: "OPTIONAL FINDING", Kind: "observation", Runs: []int{1}}},
			OpenHypotheses: []string{"OPTIONAL HYPOTHESIS"},
			Limitations:    []string{investigation.MissingEvidenceStatement}},
		history: investigation.HistoryPage{NextBefore: 3, Exchange: []investigation.BriefMessage{{
			Sequence: 5, InvestigationID: prior, Text: "The earlier value was v1.",
			Answer: &investigation.Conclusion{Findings: []investigation.Finding{{
				Statement: "The earlier value was v1.", Kind: investigation.FindingObservation, RunRefs: []int{1},
			}}},
		}}},
	}
	before, _ := json.Marshal(store.brief)
	continuation := []byte("provider-native-continuation")
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		text := prompt.System[1].Text + prompt.Content[0].Text
		for _, retained := range []string{"TASK follow_up", "beforeSequence=7", "beforeSequence=3",
			"current turn 2", "Production", "alice", store.messages[0].Text, investigation.MissingEvidenceStatement} {
			if !strings.Contains(text, retained) {
				t.Fatalf("request %d lost %q", call, retained)
			}
		}
		if prompt.ForceTool != "" || prompt.MaxOutputTokens != 1024 {
			t.Fatal("optional recovery forced finalization or changed the output allowance")
		}
		if call <= 2 && !strings.Contains(text, "OPTIONAL EXCHANGE") {
			t.Fatal("normal requests lost existing context before a provider rejection")
		}
		if call >= 3 && strings.Contains(text, "OPTIONAL") {
			t.Fatal("recovery did not retain its reduced model-visible view")
		}
		if call >= 2 {
			if len(prompt.Turns) == 0 || !bytes.Equal(prompt.Turns[0].Assistant.Raw, continuation) ||
				prompt.Turns[0].Assistant.Calls[0].ID != "read-production" ||
				len(prompt.Turns[0].ToolResults) != 1 || prompt.Turns[0].ToolResults[0].CallID != "read-production" ||
				prompt.Turns[0].ToolResults[0].IsError {
				t.Fatal("recovery lost a completed Tool exchange or native continuation")
			}
		}
		switch call {
		case 1:
			return Completion{Stop: StopToolUse, Raw: continuation, ToolCalls: []CompletionCall{{
				ID: "read-production", Name: "stub_read", Arguments: json.RawMessage(`{"input":{}}`),
			}}}, nil
		case 2:
			return Completion{Usage: TokenUsage{Input: Counted(5), Output: Counted(1)}},
				ContextRejected("scripted", "test", "context rejected", nil)
		case 3:
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "retrieve-prior", Name: historyToolName, Arguments: json.RawMessage(`{"beforeSequence":7}`),
			}}}, nil
		case 4:
			if len(prompt.Turns) != 2 || prompt.Turns[1].ToolResults[0].CallID != "retrieve-prior" ||
				!strings.Contains(prompt.Turns[1].ToolResults[0].Content, prior.String()) {
				t.Fatal("history recovery did not return the prior evidence with its identity")
			}
			var conclusion map[string]any
			if err := json.Unmarshal(validConclusion(t, []int{1}), &conclusion); err != nil {
				t.Fatal(err)
			}
			conclusion["findings"].([]any)[0].(map[string]any)["evidence_refs"] =
				[]investigation.EvidenceRef{{InvestigationID: prior, ToolRunOrdinal: 1}}
			encoded, _ := json.Marshal(conclusion)
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: encoded}}}, nil
		default:
			t.Fatal("recovery made an unexpected additional request")
			return Completion{}, nil
		}
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{Summary: "The deployed value is v2."}, nil
		}))
	if err := runner.Run(context.Background(), uuid.New(), investigation.Investigation{
		ID: uuid.New(), ConversationID: uuid.New(), Turn: 2,
		WindowFrom: time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(store.brief)
	if !bytes.Equal(before, after) || store.status != investigation.StatusConcluded || store.stoppedBy != "" ||
		model.calls != 4 || len(store.runs) != 1 || store.runs[0].IntegrationID != store.candidate.ID ||
		len(store.conclusion.Findings) != 1 || len(store.conclusion.Findings[0].EvidenceRefs) != 1 ||
		len(store.conclusion.Limitations) != 1 || store.usage.InputTokens != 5 || store.usage.OutputTokens != 1 {
		t.Fatalf("recovery lost authoritative context, provenance or accounting: status=%s calls=%d failure=%s", store.status, model.calls, store.failure)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestModelMayReadOnlyEarlierConversationHistory(t *testing.T) {
	for _, before := range []int64{3, 21} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			owner := uuid.MustParse("11111111-1111-4111-8111-111111111111")
			store := &records{
				messages: []investigation.AssignedMessage{{Sequence: 20, Text: "recall the earlier correction"}},
				history: investigation.HistoryPage{Exchange: []investigation.BriefMessage{
					{Sequence: 2, Text: "correction: staging", FromPerson: true},
					{InvestigationID: owner, Answer: &investigation.Conclusion{Findings: []investigation.Finding{{Statement: "staging was affected", Sources: []int{1}}}}},
				}},
			}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if call == 1 {
					offered := false
					for _, tool := range prompt.Tools {
						offered = offered || tool.Name == "read_conversation_history"
					}
					if !offered {
						t.Fatal("history unavailable without an external Integration")
					}
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "history", Name: "read_conversation_history",
						Arguments: json.RawMessage(fmt.Sprintf(`{"beforeSequence":%d}`, before))}}}, nil
				}
				encoded, err := json.Marshal(prompt.Turns)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "correction: staging") != (before == 3) {
					t.Fatalf("history scope mismatch: %s", encoded)
				}
				conclusion := validConclusion(t, nil)
				if before == 3 {
					conclusion = conclusionWithEvidence(t, owner)
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: conclusion}}}, nil
			}}
			runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				t.Fatal("history caused an external read")
				return integrations.ToolResult{}, nil
			}))
			org, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
			if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(), Subject: "history"}); err != nil {
				t.Fatal(err)
			}
			if store.status != investigation.StatusConcluded || len(store.runs) != 0 {
				t.Fatalf("history was treated as fresh external evidence: status=%v runs=%d failure=%q", store.status, len(store.runs), store.failure)
			}
		})
	}
}

func conclusionWithEvidence(t *testing.T, owner uuid.UUID) json.RawMessage {
	t.Helper()
	document := fmt.Sprintf(`{
		"status":"answer_only", "summary":"Earlier observations remain available.",
		"impact":{"status":"unknown","current_state":"unknown","summary":"Impact is unknown.","run_refs":[]},
		"findings":[{"id":"prior","statement":"Staging was affected.","kind":"observation",
		"confidence":"confirmed","mechanism":"","run_refs":[],
		"evidence_refs":[{"investigationId":%q,"toolRunOrdinal":1}]}]
	}`, owner)
	return json.RawMessage(document)
}

func TestHistoryAuthorityDoesNotShrinkAfterAnEarlierJump(t *testing.T) {
	t.Parallel()
	org, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	store := &records{history: investigation.HistoryPage{NextBefore: 2}}
	runner := &Agent{Store: store}
	state := &runState{organization: org, opened: investigation.Investigation{ConversationID: uuid.New()},
		historyBefore: 20, maxRuns: 2}
	for _, before := range []int64{3, 10} {
		run := runner.readHistory(context.Background(), state, toolCall{Arguments: map[string]any{"beforeSequence": before}})
		if run.Outcome != investigation.RunSucceeded {
			t.Fatalf("authorized history before %d was refused: %s", before, run.Error)
		}
	}
}

func TestRenderedHistoryKeepsCursorAndCompleteEntriesWhenBounded(t *testing.T) {
	t.Parallel()
	page := investigation.HistoryPage{NextBefore: 1, Exchange: []investigation.BriefMessage{
		{Sequence: 2, Text: "older question"},
		{Text: "older answer", Answer: &investigation.Conclusion{Summary: strings.Repeat("x", maxRunContentBytes*2)}},
		{Sequence: 3, Text: "newer correction"},
	}}
	rendered := renderResult(toolFeedback{CallID: "history", Semantic: true, Run: investigation.ToolRun{
		Tool: historyToolName, Outcome: investigation.RunSucceeded, Content: page,
	}}).Content
	jsonStart := strings.Index(rendered, "{")
	if !strings.Contains(rendered, `"nextBefore":3`) || !strings.Contains(rendered, "newer correction") ||
		!strings.Contains(rendered, `"truncated":true`) || jsonStart < 0 ||
		!json.Valid([]byte(rendered[jsonStart:])) {
		t.Fatalf("bounded history lost its cursor or complete records: %s", rendered)
	}
	continued := boundHistoryPage(investigation.HistoryPage{NextBefore: 1, Exchange: page.Exchange[:2]})
	if len(continued.Exchange) != 2 || continued.NextBefore != 1 || continued.Exchange[1].Answer != nil ||
		!strings.Contains(continued.Exchange[1].Text, "structured answer omitted") {
		t.Fatalf("continued page did not preserve the answer boundary: %+v", continued)
	}
	pair := boundHistoryPage(investigation.HistoryPage{NextBefore: 1, Exchange: []investigation.BriefMessage{
		{Sequence: 2, Text: strings.Repeat("m", investigation.BriefMessageBound)},
		{Sequence: 2, Text: "large answer", Answer: &investigation.Conclusion{Summary: strings.Repeat("a", maxRunContentBytes-1800)}},
	}})
	if len(pair.Exchange) != 2 || pair.Exchange[1].Answer != nil || pair.NextBefore != 1 {
		t.Fatalf("answer and assigned Message were split across pages: %+v", pair)
	}
}

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestModelReceivesOrderedExchangeWithCanonicalAnswerIdentity(t *testing.T) {
	answerID := uuid.New()
	at := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	store := &records{messages: []investigation.AssignedMessage{{Sequence: 2, Text: "refresh staging"}}}
	store.brief.Recent = []investigation.BriefMessage{
		{FromPerson: true, Actor: "Operator", Sequence: 1, CreatedAt: at, Text: "question-marker"},
		{CreatedAt: at.Add(time.Minute), InvestigationID: answerID, Text: "answer-marker"},
		{FromPerson: true, Actor: "Operator", Sequence: 2, CreatedAt: at.Add(2 * time.Minute), Text: "correction-marker"},
	}
	model := &scriptedModel{next: func(_ int, prompt Prompt) (Completion, error) {
		var rendered strings.Builder
		for _, blocks := range [][]Block{prompt.System, prompt.Content} {
			for _, block := range blocks {
				rendered.WriteString(block.Text)
			}
		}
		text := rendered.String()
		if strings.Contains(text, "without re-reading") || !strings.Contains(text, "refresh") {
			t.Fatalf("follow-up prompt prevents an explicitly requested refresh: %s", text)
		}
		for _, required := range []string{answerID.String(), "2026-09-06T10:01:00Z", "message 2"} {
			if !strings.Contains(text, required) {
				t.Fatalf("model context omitted exchange identity %q", required)
			}
		}
		if strings.Index(text, "question-marker") >= strings.Index(text, "answer-marker") ||
			strings.Index(text, "answer-marker") >= strings.Index(text, "correction-marker") {
			t.Fatal("model context reordered the exchange")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}))
	org, _ := tenancy.NewOrganization("org-test")
	if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(), Turn: 2, Subject: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls=%d", model.calls)
	}
}

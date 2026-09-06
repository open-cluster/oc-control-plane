package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestRunRefusesWhenConversationOriginCannotBeVerified(t *testing.T) {
	for name, store := range map[string]*records{
		"read failure":      {originErr: errors.New("provider binding unavailable")},
		"incomplete origin": {origin: &investigation.ConversationOrigin{IntegrationID: uuid.New()}},
	} {
		t.Run(name, func(t *testing.T) {
			model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
					ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
				}}}, nil
			}}
			catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				t.Fatal("external read before origin verification")
				return integrations.ToolResult{}, nil
			})
			runner := configuredTestAgent(t, store, model, catalog)
			org, _ := tenancy.NewOrganization("org-test")
			if err := runner.Run(context.Background(), org, investigation.Investigation{
				ID: uuid.New(), ConversationID: uuid.New(), Subject: "question",
			}); err != nil {
				t.Fatal(err)
			}
			if model.calls != 0 || store.status != investigation.StatusFailed {
				t.Fatalf("unverified origin: model calls=%d, status=%v", model.calls, store.status)
			}
		})
	}
}

func TestFirstQuestionReportsUnavailableHistoryWithoutBecomingAFollowUp(t *testing.T) {
	store := &records{briefErr: errors.New("history unavailable")}
	model := &scriptedModel{next: func(_ int, prompt Prompt) (Completion, error) {
		var rendered strings.Builder
		for _, blocks := range [][]Block{prompt.System, prompt.Content} {
			for _, block := range blocks {
				rendered.WriteString(block.Text)
			}
		}
		text := rendered.String()
		if !strings.Contains(text, "Previous Conversation history could not be loaded") {
			t.Fatal("optional history failure has no model-visible limitation")
		}
		if !strings.Contains(text, "TASK operator_question:") || strings.Contains(text, "TASK follow_up:") ||
			strings.Contains(text, "not a fresh investigation") {
			t.Fatal("a first question was classified as a follow-up")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}))
	org, _ := tenancy.NewOrganization("org-test")
	if err := runner.Run(context.Background(), org, investigation.Investigation{
		ID: uuid.New(), ConversationID: uuid.New(), Turn: 1, Subject: "question", Question: "What changed?",
	}); err != nil {
		t.Fatal(err)
	}
}

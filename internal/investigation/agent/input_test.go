package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestMissingAssignedInputDoesNotFallBackToQuestionPreview(t *testing.T) {
	for name, failure := range map[string]error{"missing": nil, "unavailable": errors.New("input unavailable")} {
		t.Run(name, func(t *testing.T) {
			store := &records{messagesErr: failure}
			model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName,
					Arguments: validConclusion(t, nil)}}}, nil
			}}
			runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				t.Fatal("external read before required input")
				return integrations.ToolResult{}, nil
			}))
			org, _ := tenancy.NewOrganization("org-test")
			if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(),
				Subject: "question", Question: "truncated preview"}); err != nil {
				t.Fatal(err)
			}
			if model.calls != 0 || store.status != investigation.StatusFailed {
				t.Fatalf("missing input: model calls=%d status=%v", model.calls, store.status)
			}
		})
	}
}

func TestAllAssignedSequencesAreReportedWhenBatchCannotFit(t *testing.T) {
	store := &records{}
	for n := range 15 {
		store.messages = append(store.messages, investigation.AssignedMessage{Sequence: int64(n + 20), Text: strings.Repeat("界", 8192)})
	}
	model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
		t.Fatal("oversized batch reached model")
		return Completion{}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		t.Fatal("oversized batch triggered an external read")
		return integrations.ToolResult{}, nil
	}))
	org, _ := tenancy.NewOrganization("org-test")
	if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(), Subject: "batch"}); err != nil {
		t.Fatal(err)
	}
	if len(store.conclusion.Limitations) != 1 {
		t.Fatal("no unprocessed input limitation")
	}
	sequences := store.conclusion.Limitations[0].MessageSequences
	if len(sequences) != 15 {
		t.Fatalf("unprocessed sequences=%v", sequences)
	}
	for n, sequence := range sequences {
		if sequence != int64(n+20) {
			t.Fatalf("wrong unprocessed sequence: %v", sequences)
		}
	}
}

func TestAssignedBatchBeyondHistoryTailSurvivesOptionalContextTrimming(t *testing.T) {
	store := &records{}
	for n := range 40 {
		store.brief.Findings = append(store.brief.Findings, investigation.PriorFinding{Statement: fmt.Sprintf("%d ", n) + strings.Repeat("old-untrusted-finding ", 50)})
	}
	for n := range 15 {
		store.messages = append(store.messages, investigation.AssignedMessage{Sequence: int64(n + 1), Actor: "Operator",
			Text: fmt.Sprintf("ordered-request-%02d", n)})
	}
	model := &scriptedModel{next: func(_ int, prompt Prompt) (Completion, error) {
		var text strings.Builder
		for _, block := range prompt.Content {
			text.WriteString(block.Text)
		}
		if strings.Contains(text.String(), "old-untrusted-finding") || !strings.Contains(text.String(), "Optional history and inventory were omitted") {
			t.Fatal("optional context was not explicitly trimmed before current input")
		}
		previous := -1
		for n := range 15 {
			at := strings.Index(text.String(), fmt.Sprintf("ordered-request-%02d", n))
			if at <= previous {
				t.Fatalf("assigned request %d missing or out of order", n)
			}
			previous = at
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}))
	runner.ContextWindowTokens = 40000
	org, _ := tenancy.NewOrganization("org-test")
	if err := runner.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(), Subject: "batch"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls=%d", model.calls)
	}
}

package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"strings"
	"testing"
)

func TestRequestBudgetCoversTheSentBody(t *testing.T) {
	prompt := toolPrompt()
	prompt.System = append(prompt.System, reasoning.Block{Text: strings.Repeat("界", 1000)})
	prompt.Tools[0].Description = strings.Repeat("schema ", 2000)
	prompt.Turns = []reasoning.Turn{{Assistant: reasoning.AssistantTurn{Calls: []reasoning.CompletionCall{{
		ID: "read-1", Name: prompt.Tools[0].Name, Arguments: json.RawMessage("{}"),
	}}}, Results: []reasoning.ToolResultTurn{{CallID: "read-1", Content: strings.Repeat("result ", 1000)}}}}
	for _, forced := range []bool{false, true} {
		if forced {
			prompt.ForceTool = prompt.Tools[0].Name
		}
		provider, wire := providerUnder(t, streamed("{}", fullUsage, "end_turn", ""))
		estimate, err := provider.RequestTokens(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Complete(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
		body := wire.lastBody(t)
		if estimate < len(body) || estimate > len(body)*2 {
			t.Fatalf("forced=%t estimate=%d body bytes=%d", forced, estimate, len(body))
		}
	}
}
func TestContextRejectionAllowsBoundedRecovery(t *testing.T) {
	provider, _ := providerUnder(t, failedWith(400, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"prompt is too long for this context window"}}`))
	_, err := provider.Complete(context.Background(), promptFixture())
	if !errors.Is(err, reasoning.ErrContextWindow) {
		t.Fatalf("error = %v", err)
	}
}

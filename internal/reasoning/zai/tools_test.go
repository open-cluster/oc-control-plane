package zai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Native function calling on the wire: the generated definitions travel as this
// vendor's function tools, tool_calls decode into this system's calls, and a replayed
// turn is rebuilt as assistant tool_calls plus role-"tool" results.

func toolDefinitions() []integrations.ToolDefinition {
	channelTool := integrations.Tool{
		Name:        "slack.list_channels",
		Description: "Lists the channels.", WhenToUse: "First.",
		WhenNotToUse: "Twice.", Permissions: "channels:read", Output: "Channels.",
		Arguments: []integrations.ToolArgument{{
			Name: "filter", Description: "a name filter",
			Type: integrations.FieldString, Required: true,
		}},
	}
	return []integrations.ToolDefinition{
		channelTool.Definition(),
		reasoning.ConcludeDefinition(),
	}
}

func toolPrompt() reasoning.Prompt {
	return reasoning.Prompt{
		Model:           "glm-4.7",
		System:          []reasoning.Block{{Text: "the frozen preamble", Cache: true}},
		Content:         []reasoning.Block{{Text: "the orientation", Cache: true}},
		Tools:           toolDefinitions(),
		MaxOutputTokens: 32_000,
		Effort:          reasoning.EffortHigh,
	}
}

func toolCallAnswer() *http.Response {
	return answered(200, `{
		"id": "chat-1", "request_id": "req-1", "model": "glm-4.7",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": "",
			"tool_calls": [{"id": "call_abc", "type": "function", "function": {
				"name": "slack.list_channels",
				"arguments": "{\"filter\":\"incid\"}"
			}}]
		}}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
	}`)
}

func TestComplete_SendsTheGeneratedFunctionTools(t *testing.T) {
	provider, round := providerUnder(t, toolCallAnswer())

	if _, err := provider.Complete(context.Background(), toolPrompt()); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice     any            `json:"tool_choice"`
		ResponseFormat map[string]any `json:"response_format"`
		Messages       []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(round.lastBody(t)), &sent); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if len(sent.Tools) != 2 || sent.Tools[0].Type != "function" ||
		sent.Tools[0].Function.Name != "slack.list_channels" ||
		sent.Tools[1].Function.Name != "conclude" {
		t.Fatalf("tools = %+v", sent.Tools)
	}
	if !strings.Contains(sent.Tools[0].Function.Description, "Use when: First.") {
		t.Errorf("description = %q", sent.Tools[0].Function.Description)
	}
	if sent.Tools[0].Function.Parameters["type"] != "object" {
		t.Errorf("parameters = %+v; the generated schema travels verbatim",
			sent.Tools[0].Function.Parameters)
	}
	if sent.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v", sent.ToolChoice)
	}
	if sent.ResponseFormat != nil {
		t.Error("a tool-calling prompt declares no response format; the conclude tool " +
			"is the contract")
	}
	// The schema-in-prompt workaround belongs to document mode alone.
	for _, message := range sent.Messages {
		if strings.Contains(message.Content, "JSON Schema") {
			t.Error("the schema workaround leaked into a tool-calling prompt")
		}
	}
}

func TestComplete_DecodesToolCallsIntoCalls(t *testing.T) {
	provider, _ := providerUnder(t, toolCallAnswer())

	completion, err := provider.Complete(context.Background(), toolPrompt())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completion.Stop != reasoning.StopToolUse {
		t.Fatalf("stop = %s", completion.Stop)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "call_abc" ||
		completion.ToolCalls[0].Name != "slack.list_channels" {
		t.Fatalf("calls = %+v", completion.ToolCalls)
	}
	var arguments map[string]any
	if err := json.Unmarshal(completion.ToolCalls[0].Arguments, &arguments); err != nil ||
		arguments["filter"] != "incid" {
		t.Errorf("arguments = %s", completion.ToolCalls[0].Arguments)
	}
}

func TestComplete_ReplaysTheTurnAsToolCallsAndToolResults(t *testing.T) {
	provider, round := providerUnder(t, toolCallAnswer(), answered(200, `{
		"id": "chat-2", "model": "glm-4.7",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": "",
			"tool_calls": [{"id": "call_done", "type": "function", "function": {
				"name": "conclude",
				"arguments": "{\"findings\":[],\"next_steps\":[]}"
			}}]
		}}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
	}`))

	first, err := provider.Complete(context.Background(), toolPrompt())
	if err != nil {
		t.Fatalf("first complete: %v", err)
	}

	followUp := toolPrompt()
	followUp.Turns = []reasoning.Turn{{
		Assistant: reasoning.AssistantTurn{Calls: first.ToolCalls, Raw: first.Raw},
		Results: []reasoning.ToolResultTurn{{
			CallID: "call_abc", Content: "[run 1] 2 channels",
		}},
		Instruction: "No further reads are available. Conclude now.",
	}}
	followUp.ForceTool = reasoning.ConcludeToolName
	if _, err := provider.Complete(context.Background(), followUp); err != nil {
		t.Fatalf("second complete: %v", err)
	}

	var sent struct {
		ToolChoice map[string]any `json:"tool_choice"`
		Messages   []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(round.lastBody(t)), &sent); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if function, isMap := sent.ToolChoice["function"].(map[string]any); !isMap ||
		sent.ToolChoice["type"] != "function" || function["name"] != "conclude" {
		t.Errorf("tool_choice = %v; the forced turn names the one callable tool", sent.ToolChoice)
	}
	// system, user orientation, assistant tool_calls, tool result, user instruction.
	if len(sent.Messages) != 5 {
		t.Fatalf("messages = %d: %+v", len(sent.Messages), sent.Messages)
	}
	assistant := sent.Messages[2]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 ||
		assistant.ToolCalls[0].ID != "call_abc" ||
		assistant.ToolCalls[0].Function.Name != "slack.list_channels" {
		t.Fatalf("assistant turn = %+v", assistant)
	}
	result := sent.Messages[3]
	if result.Role != "tool" || result.ToolCallID != "call_abc" ||
		!strings.Contains(result.Content, "2 channels") {
		t.Errorf("result turn = %+v", result)
	}
	instruction := sent.Messages[4]
	if instruction.Role != "user" || !strings.Contains(instruction.Content, "Conclude now") {
		t.Errorf("instruction turn = %+v", instruction)
	}
}

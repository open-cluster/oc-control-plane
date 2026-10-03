package zai

import (
	"encoding/json"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

type request struct {
	Model           string          `json:"model"`
	Messages        []message       `json:"messages"`
	MaxTokens       int64           `json:"max_tokens"`
	Stream          bool            `json:"stream"`
	Thinking        *thinking       `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	Tools           []functionTool  `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type functionTool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type thinking struct {
	Type string `json:"type"`
}

type responseFormat struct {
	Type string `json:"type"`
}

func (p *Provider) request(prompt reasoning.Prompt) request {
	body := request{
		Model:           prompt.Model,
		MaxTokens:       prompt.MaxOutputTokens,
		Stream:          false,
		Thinking:        &thinking{Type: "enabled"},
		ReasoningEffort: effortOf(prompt.Effort),
	}
	if len(prompt.Tools) == 0 {
		body.Messages = []message{
			{Role: "system", Content: joined(prompt.System)},
			{Role: "user", Content: userContent(prompt)},
		}
		// JSON mode prevents prose wrapping but does not enforce the schema embedded in the prompt.
		body.ResponseFormat = &responseFormat{Type: "json_object"}
		return body
	}

	body.Messages = conversationMessages(prompt)
	body.Tools = functionTools(prompt.Tools)
	if prompt.ForceTool != "" {
		body.ToolChoice = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": prompt.ForceTool},
		}
	} else {
		body.ToolChoice = "auto"
	}
	return body
}

func conversationMessages(prompt reasoning.Prompt) []message {
	rendered := []message{
		{Role: "system", Content: joined(prompt.System)},
		{Role: "user", Content: joined(prompt.Content)},
	}
	for _, turn := range prompt.Turns {
		rendered = append(rendered, assistantMessage(turn.Assistant))
		for _, result := range turn.Results {
			rendered = append(rendered, message{
				Role: "tool", Content: result.Content, ToolCallID: result.CallID,
			})
		}
		if turn.Instruction != "" {
			rendered = append(rendered, message{Role: "user", Content: turn.Instruction})
		}
	}
	return rendered
}

func assistantMessage(assistant reasoning.AssistantTurn) message {
	calls := make([]toolCall, 0, len(assistant.Calls))
	for _, call := range assistant.Calls {
		calls = append(calls, toolCall{
			ID: call.ID, Type: "function",
			Function: functionCall{Name: call.Name, Arguments: string(call.Arguments)},
		})
	}
	return message{Role: "assistant", Content: assistant.Text, ToolCalls: calls}
}

func functionTools(definitions []integrations.ToolDefinition) []functionTool {
	tools := make([]functionTool, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, functionTool{
			Type: "function",
			Function: function{
				Name:        definition.Name,
				Description: definition.Description,
				Parameters:  definition.InputSchema,
			},
		})
	}
	return tools
}

func userContent(prompt reasoning.Prompt) string {
	content := &strings.Builder{}
	content.WriteString(joined(prompt.Content))
	content.WriteString("\n\nReturn one JSON object and nothing else — no prose, no markdown, no " +
		"code fence. It must match this JSON Schema exactly, including every required field:\n\n")
	content.Write(renderedSchema(prompt.Schema))
	return content.String()
}

func renderedSchema(schema reasoning.Schema) []byte {
	encoded, err := json.MarshalIndent(schema.Document, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

func joined(blocks []reasoning.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

func effortOf(effort reasoning.Effort) string {
	switch effort {
	case reasoning.EffortLow:
		return "low"
	case reasoning.EffortMedium:
		return "medium"
	case reasoning.EffortExtraHigh, reasoning.EffortMax:
		return "max"
	default:
		return "high"
	}
}

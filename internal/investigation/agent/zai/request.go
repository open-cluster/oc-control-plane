package zai

import (
	"encoding/json"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Turning a provider-neutral prompt into this vendor's request.
//
// The interesting part is what this adapter has to do that the other one does not. This vendor's
// JSON mode returns valid JSON but does not enforce a schema, so the schema is rendered into the
// prompt here — at the very end, after everything cacheable, because a schema appended before the
// cache boundary would move bytes in the prefix.
//
// Doing it here rather than in the shared prompt keeps the workaround with the provider that
// is missing the capability, so a vendor that enforces schemas is not charged for one that
// does not.

// request is this vendor's chat completion body.
type request struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int64     `json:"max_tokens"`
	// Stream is explicitly false. The usage figures arrive complete in one body this way, and a
	// cost figure that is silently absent disables the cost ceiling without saying so.
	Stream          bool            `json:"stream"`
	Thinking        *thinking       `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	Tools           []functionTool  `json:"tools,omitempty"`
	// ToolChoice is "auto", or {"type":"function","function":{"name":...}} on the
	// forced concluding turn.
	ToolChoice any `json:"tool_choice,omitempty"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// functionTool is this vendor's OpenAI-style tool declaration. Parameters carry the
// generated input schema verbatim.
type functionTool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toolCall is one native call as this vendor speaks it, in requests and responses
// alike: the arguments are a JSON document in a string.
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

// request builds the body for one prompt. A document prompt keeps the schema-in-prompt
// workaround and the JSON response mode; a tool-calling prompt declares the generated
// tools instead — the conclude tool's parameters are that mode's output contract, and
// forcing a JSON response mode would fight the tool-call answer shape.
func (p *Provider) request(prompt reasoning.Prompt) request {
	body := request{
		Model:     prompt.Model,
		MaxTokens: prompt.MaxOutputTokens,
		Stream:    false,
		// Thinking is asked for explicitly on this vendor, where the other one has it on by
		// default. Depth is the effort level rather than a token budget.
		Thinking:        &thinking{Type: "enabled"},
		ReasoningEffort: effortOf(prompt.Effort),
	}
	if len(prompt.Tools) == 0 {
		body.Messages = []message{
			{Role: "system", Content: joined(prompt.System)},
			{Role: "user", Content: userContent(prompt)},
		}
		// A JSON mode rather than a schema. It stops the model wrapping the answer in prose; it
		// does not stop the answer being the wrong shape, which is what the decoder is for.
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

// conversationMessages renders the transcript: system, the orientation, then each
// turn — the assistant's calls, one role-"tool" message per result, and any trailing
// instruction as a user message.
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

// assistantMessage rebuilds one prior assistant turn from the neutral fields. This
// vendor's conversation is stateless text, so nothing needs the captured raw form.
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

// functionTools translates the generated definitions into this vendor's tool shape.
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

// userContent renders the deliberation and appends the schema the answer must match.
//
// The schema goes last, after everything the prompt marked cacheable, so the cacheable prefix is
// byte-identical to what it would have been on a provider that enforces schemas natively.
func userContent(prompt reasoning.Prompt) string {
	content := &strings.Builder{}
	content.WriteString(joined(prompt.Content))
	content.WriteString("\n\nReturn one JSON object and nothing else — no prose, no markdown, no " +
		"code fence. It must match this JSON Schema exactly, including every required field:\n\n")
	content.Write(renderedSchema(prompt.Schema))
	return content.String()
}

// renderedSchema writes the schema deterministically. Go's JSON encoder sorts map keys, so the
// same schema renders the same bytes every time — which matters because these bytes are part of
// what a prompt cache would key on.
func renderedSchema(schema reasoning.Schema) []byte {
	encoded, err := json.MarshalIndent(schema.Document, "", "  ")
	if err != nil {
		// The schemas are compiled-in literals, so this cannot happen from configuration. Saying
		// so beats returning a silent empty schema that would make every answer the wrong shape.
		return []byte("{}")
	}
	return encoded
}

// joined concatenates the prompt's blocks. This vendor takes whole messages rather than content
// blocks, so the cache boundaries the blocks carry are not expressible on the wire — the ordering
// they enforce still is, and that is what keeps the prefix stable.
func joined(blocks []reasoning.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

// effortOf maps this system's effort vocabulary onto the vendor's, which offers a finer scale at
// the bottom than this system uses.
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

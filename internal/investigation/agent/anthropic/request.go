package anthropic

import (
	"encoding/json"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Turning a provider-neutral prompt into this vendor's request.
//
// Three decisions are load-bearing and each of them is about this model specifically. Thinking is
// adaptive and its depth is set by effort rather than a token budget — the fixed-budget parameter
// is removed on this model and sending it is refused outright. Sampling parameters are removed too,
// so steering happens in the prompt and nowhere else. The answer is constrained by a declared JSON
// schema rather than parsed out of prose, which is why there are no tool definitions here at all.

// params builds the request for one prompt. A document prompt declares its output
// schema; a tool-calling prompt declares the tool definitions instead — the conclude
// tool's input schema is that mode's output contract, so declaring both would say two
// things about one answer.
func (p *Provider) params(prompt reasoning.Prompt) sdk.MessageNewParams {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(prompt.Model),
		MaxTokens: prompt.MaxOutputTokens,
		System:    systemBlocks(prompt),
		Messages:  messages(prompt),
		// Thinking is on by default on this model; asking for it explicitly says so in the code
		// rather than leaving the next reader to know it. Display stays omitted because nothing
		// here reads the reasoning: the document is the answer, and the round records the
		// reasoning token count rather than the reasoning.
		Thinking: sdk.ThinkingConfigParamUnion{
			OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: sdk.OutputConfigParam{
			Effort: effortOf(prompt.Effort),
		},
	}
	if len(prompt.Tools) == 0 {
		params.OutputConfig.Format = sdk.JSONOutputFormatParam{Schema: prompt.Schema.Document}
		return params
	}
	params.Tools = toolParams(prompt.Tools)
	if prompt.ForceTool != "" {
		params.ToolChoice = sdk.ToolChoiceUnionParam{
			OfTool: &sdk.ToolChoiceToolParam{Name: prompt.ForceTool},
		}
	} else {
		params.ToolChoice = sdk.ToolChoiceUnionParam{
			OfAuto: &sdk.ToolChoiceAutoParam{},
		}
	}
	return params
}

// toolParams translates the generated definitions into this vendor's tool shape,
// verbatim: the input schema's properties, required list and closed-over
// additionalProperties travel exactly as the one declarative contract rendered them.
func toolParams(definitions []integrations.ToolDefinition) []sdk.ToolUnionParam {
	tools := make([]sdk.ToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		schema := sdk.ToolInputSchemaParam{
			Properties: definition.InputSchema["properties"],
		}
		if required, listed := definition.InputSchema["required"].([]any); listed {
			for _, name := range required {
				if text, isText := name.(string); isText {
					schema.Required = append(schema.Required, text)
				}
			}
		}
		if additional, closed := definition.InputSchema["additionalProperties"]; closed {
			schema.ExtraFields = map[string]any{"additionalProperties": additional}
		}
		tools = append(tools, sdk.ToolUnionParam{OfTool: &sdk.ToolParam{
			Name:        definition.Name,
			Description: sdk.String(definition.Description),
			InputSchema: schema,
		}})
	}
	return tools
}

// messages renders the conversation: the orientation as the first user message, then
// each completed turn — the assistant's own move replayed verbatim, and the results
// with any trailing instruction as the user message that answered it.
func messages(prompt reasoning.Prompt) []sdk.MessageParam {
	rendered := []sdk.MessageParam{sdk.NewUserMessage(contentBlocks(prompt)...)}
	for _, turn := range prompt.Turns {
		// A turn with no assistant content cannot render an assistant message this
		// vendor would accept; its results and instruction still say what happened.
		if assistant := assistantMessage(turn.Assistant); len(assistant.Content) > 0 {
			rendered = append(rendered, assistant)
		}
		if len(turn.Results) > 0 || turn.Instruction != "" {
			blocks := make([]sdk.ContentBlockParamUnion, 0, len(turn.Results)+1)
			for _, result := range turn.Results {
				blocks = append(blocks, sdk.NewToolResultBlock(
					result.CallID, result.Content, result.IsError))
			}
			if turn.Instruction != "" {
				blocks = append(blocks, sdk.NewTextBlock(turn.Instruction))
			}
			rendered = append(rendered, sdk.NewUserMessage(blocks...))
		}
	}
	return rendered
}

// assistantMessage replays one prior assistant turn. The captured raw message is
// preferred because this vendor requires its own thinking blocks — signatures included
// — echoed back during a tool loop; the neutral fields are the fallback when no capture
// exists.
func assistantMessage(assistant reasoning.AssistantTurn) sdk.MessageParam {
	if len(assistant.Raw) > 0 {
		var captured sdk.Message
		if err := json.Unmarshal(assistant.Raw, &captured); err == nil &&
			len(captured.Content) > 0 {
			return captured.ToParam()
		}
	}
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(assistant.Calls)+1)
	if assistant.Text != "" {
		blocks = append(blocks, sdk.NewTextBlock(assistant.Text))
	}
	for _, call := range assistant.Calls {
		var input any = map[string]any{}
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal(call.Arguments, &input)
		}
		blocks = append(blocks, sdk.NewToolUseBlock(call.ID, input, call.Name))
	}
	return sdk.NewAssistantMessage(blocks...)
}

// systemBlocks renders the frozen preamble, carrying the cache breakpoint where the prompt put it.
//
// The preamble is identical across every investigation in every organization, so it caches once and
// is read by everything. The minimum cacheable prefix on this model is 512 tokens; a shorter one
// does not cache and does not say so, which is why the preamble is written as one substantial block
// rather than several small ones.
func systemBlocks(prompt reasoning.Prompt) []sdk.TextBlockParam {
	blocks := make([]sdk.TextBlockParam, 0, len(prompt.System))
	for _, block := range prompt.System {
		rendered := sdk.TextBlockParam{Text: block.Text}
		if block.Cache {
			rendered.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		blocks = append(blocks, rendered)
	}
	return blocks
}

// contentBlocks renders the deliberation, carrying the second breakpoint at the end of the brief.
//
// Ordering is the whole game. Nothing volatile may appear before the last cached block, because
// caching is a prefix match and a byte that moves anywhere in the prefix invalidates everything
// after it — silently, and at full price.
func contentBlocks(prompt reasoning.Prompt) []sdk.ContentBlockParamUnion {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(prompt.Content))
	for _, block := range prompt.Content {
		rendered := &sdk.TextBlockParam{Text: block.Text}
		if block.Cache {
			rendered.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		blocks = append(blocks, sdk.ContentBlockParamUnion{OfText: rendered})
	}
	return blocks
}

// effortOf maps this system's effort vocabulary onto the vendor's. They happen to agree on this
// model, and the table exists so that a vendor that stops agreeing is a change here rather than a
// rejected request.
func effortOf(effort reasoning.Effort) sdk.OutputConfigEffort {
	switch effort {
	case reasoning.EffortLow:
		return sdk.OutputConfigEffortLow
	case reasoning.EffortMedium:
		return sdk.OutputConfigEffortMedium
	case reasoning.EffortExtraHigh:
		return sdk.OutputConfigEffortXhigh
	case reasoning.EffortMax:
		return sdk.OutputConfigEffortMax
	default:
		return sdk.OutputConfigEffortHigh
	}
}

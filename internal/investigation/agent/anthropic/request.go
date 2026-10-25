package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

func (p *Provider) params(prompt reasoning.Prompt) sdk.MessageNewParams {
	params := sdk.MessageNewParams{
		Model:     prompt.Model,
		MaxTokens: prompt.MaxOutputTokens,
		System:    systemBlocks(prompt),
		Messages:  messages(prompt),
		Thinking:  thinkingOf(prompt.Model, prompt.Effort, prompt.MaxOutputTokens),
	}
	if params.Thinking.OfAdaptive != nil || modelIs(prompt.Model, sdk.ModelClaudeOpus4_5) {
		params.OutputConfig.Effort = effortOf(prompt.Effort)
	}
	if len(prompt.Tools) == 0 {
		params.OutputConfig.Format = sdk.JSONOutputFormatParam{Schema: prompt.Schema.Document}
		return params
	}
	params.Tools = toolParams(prompt.Tools)
	// Manual thinking and these documented model exceptions require automatic Tool choice.
	canForce := params.Thinking.OfEnabled == nil && !modelIs(prompt.Model,
		"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-mythos-5-1")
	if prompt.ForceTool != "" && canForce {
		params.ToolChoice = sdk.ToolChoiceUnionParam{
			OfTool: &sdk.ToolChoiceToolParam{Name: anthropicToolName(prompt.ForceTool)},
		}
	} else {
		params.ToolChoice = sdk.ToolChoiceUnionParam{
			OfAuto: &sdk.ToolChoiceAutoParam{},
		}
	}
	return params
}

func modelIs(model string, families ...string) bool {
	for _, family := range families {
		if model == family {
			return true
		}
		if snapshot, ok := strings.CutPrefix(model, family+"-"); ok && len(snapshot) == 8 {
			if _, err := strconv.Atoi(snapshot); err == nil {
				return true
			}
		}
	}
	return false
}

func validateEffort(model string, effort reasoning.Effort) error {
	if effort == reasoning.EffortMax && modelIs(model, sdk.ModelClaudeOpus4_5) ||
		effort == reasoning.EffortExtraHigh && modelIs(model,
			sdk.ModelClaudeOpus4_5, sdk.ModelClaudeOpus4_6, sdk.ModelClaudeSonnet4_6) {
		return fmt.Errorf("the selected Anthropic model does not support effort %q", effort)
	}
	return nil
}

func thinkingOf(model string, effort reasoning.Effort, maxTokens int64) sdk.ThinkingConfigParamUnion {
	if modelIs(model,
		sdk.ModelClaudeOpus4_6,
		sdk.ModelClaudeSonnet4_6,
		sdk.ModelClaudeOpus4_7,
		sdk.ModelClaudeOpus4_8,
		sdk.ModelClaudeOpus5,
		sdk.ModelClaudeSonnet5,
		sdk.ModelClaudeFable5,
		sdk.ModelClaudeMythos5) {
		return sdk.ThinkingConfigParamUnion{
			OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
		}
	}
	if !modelIs(model,
		sdk.ModelClaudeHaiku4_5,
		sdk.ModelClaudeSonnet5,
		sdk.ModelClaudeOpus5) {
		// Unknown identifiers retain provider defaults; they do not imply adaptive support.
		return sdk.ThinkingConfigParamUnion{}
	}
	if effort == reasoning.EffortLow || maxTokens <= 1024 {
		return sdk.ThinkingConfigParamUnion{OfDisabled: &sdk.ThinkingConfigDisabledParam{}}
	}
	budget := manualThinkingBudget(effort)
	if budget >= maxTokens {
		budget = maxTokens - 1
	}
	if budget < 1024 {
		return sdk.ThinkingConfigParamUnion{OfDisabled: &sdk.ThinkingConfigDisabledParam{}}
	}
	return sdk.ThinkingConfigParamOfEnabled(budget)
}

func manualThinkingBudget(effort reasoning.Effort) int64 {
	switch effort {
	case reasoning.EffortLow:
		return 1024
	case reasoning.EffortMedium:
		return 2048
	case reasoning.EffortExtraHigh:
		return 8192
	case reasoning.EffortMax:
		return 16_000
	default:
		return 4096
	}
}

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
			Name:        anthropicToolName(definition.Name),
			Description: sdk.String(definition.Description),
			InputSchema: schema,
		}})
	}
	return tools
}

func messages(prompt reasoning.Prompt) []sdk.MessageParam {
	rendered := []sdk.MessageParam{sdk.NewUserMessage(contentBlocks(prompt)...)}
	for _, turn := range prompt.Turns {
		if assistant := assistantMessage(turn.Assistant); len(assistant.Content) > 0 {
			rendered = append(rendered, assistant)
		}
		if len(turn.ToolResults) > 0 || turn.Instruction != "" {
			blocks := make([]sdk.ContentBlockParamUnion, 0, len(turn.ToolResults)+1)
			for _, result := range turn.ToolResults {
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

func assistantMessage(assistant reasoning.AssistantTurn) sdk.MessageParam {
	// Prefer the captured vendor message so signed thinking blocks survive tool-loop replay.
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
		blocks = append(blocks, sdk.NewToolUseBlock(call.ID, input, anthropicToolName(call.Name)))
	}
	return sdk.NewAssistantMessage(blocks...)
}

func anthropicToolName(name string) string {
	return "oc_" + base64.RawURLEncoding.EncodeToString([]byte(name))
}

func internalToolName(name string) string {
	encoded, found := strings.CutPrefix(name, "oc_")
	if !found {
		return name
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return name
	}
	return string(decoded)
}

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

func contentBlocks(prompt reasoning.Prompt) []sdk.ContentBlockParamUnion {
	// Anthropic caches byte-identical prefixes, so volatile content must follow the final breakpoint.
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

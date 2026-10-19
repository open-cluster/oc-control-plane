package openrouter

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	sdk "github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

var validToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func toolName(name string) string {
	if validToolName.MatchString(name) {
		return name
	}
	// Integration-qualified names can exceed the provider's length and character limits.
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("oc_%x", digest[:16])
}

func request(prompt agent.Prompt) (sdk.ChatRequest, error) {
	chatRequest := sdk.ChatRequest{
		Model:               &prompt.Model,
		MaxCompletionTokens: optionalnullable.From(&prompt.MaxOutputTokens),
		Stream:              new(false),
		Provider: optionalnullable.From(&sdk.ProviderPreferences{
			Sort: optionalnullable.From(new(
				sdk.CreateProviderPreferencesSortProviderSort(sdk.ProviderSortPrice))),
			AllowFallbacks:    optionalnullable.From(new(false)),
			RequireParameters: optionalnullable.From(new(true)),
			DataCollection:    optionalnullable.From(new(sdk.DataCollectionDeny)),
			Zdr:               optionalnullable.From(new(true)),
		}),
		Messages: []sdk.ChatMessages{
			sdk.CreateChatMessagesSystem(sdk.ChatSystemMessage{
				Content: sdk.CreateChatSystemMessageContentStr(joined(prompt.System)),
			}),
			sdk.CreateChatMessagesUser(sdk.ChatUserMessage{
				Content: sdk.CreateChatUserMessageContentStr(joined(prompt.Content)),
			}),
		},
		ToolChoice: new(sdk.CreateChatToolChoiceChatToolChoiceAuto(sdk.ChatToolChoiceAutoAuto)),
	}
	for _, definition := range prompt.Tools {
		chatRequest.Tools = append(chatRequest.Tools, sdk.CreateChatFunctionToolChatFunctionToolFunction(
			sdk.ChatFunctionToolFunction{
				Type: sdk.ChatFunctionToolTypeFunction,
				Function: sdk.ChatFunctionToolFunctionFunction{
					Name:        toolName(definition.Name),
					Description: &definition.Description,
					Parameters:  definition.InputSchema,
				},
			}))
	}
	if prompt.ForceTool != "" {
		chatRequest.ToolChoice = new(sdk.CreateChatToolChoiceChatNamedToolChoice(sdk.ChatNamedToolChoice{
			Type: sdk.ChatNamedToolChoiceTypeFunction,
			Function: sdk.ChatNamedToolChoiceFunction{
				Name: toolName(prompt.ForceTool),
			},
		}))
	}
	for _, turn := range prompt.Turns { //todo recheck and analyze it
		var assistant sdk.ChatAssistantMessage
		if len(turn.Assistant.Raw) > 0 {
			// Replay signed/encrypted reasoning and native calls without reconstructing them.
			if err := json.Unmarshal(turn.Assistant.Raw, &assistant); err != nil {
				return sdk.ChatRequest{}, fmt.Errorf("invalid model continuation")
			}
		} else {
			assistant.Content = optionalnullable.From(new(
				sdk.CreateChatAssistantMessageContentStr(turn.Assistant.Text)))
			for _, call := range turn.Assistant.Calls {
				assistant.ToolCalls = append(assistant.ToolCalls, sdk.ChatToolCall{
					ID:   call.ID,
					Type: sdk.ChatToolCallTypeFunction,
					Function: sdk.ChatToolCallFunction{
						Name:      toolName(call.Name),
						Arguments: string(call.Arguments)},
				})
			}
		}
		chatRequest.Messages = append(chatRequest.Messages, sdk.CreateChatMessagesAssistant(assistant))
		for _, result := range turn.Results {
			chatRequest.Messages = append(chatRequest.Messages, sdk.CreateChatMessagesTool(
				sdk.ChatToolMessage{
					ToolCallID: result.CallID,
					Content:    sdk.CreateChatToolMessageContentStr(result.Content),
				}))
		}
		if turn.Instruction != "" {
			chatRequest.Messages = append(chatRequest.Messages, sdk.CreateChatMessagesUser(sdk.ChatUserMessage{
				Content: sdk.CreateChatUserMessageContentStr(turn.Instruction),
			}))
		}
	}
	return chatRequest, nil
}

func joined(blocks []agent.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

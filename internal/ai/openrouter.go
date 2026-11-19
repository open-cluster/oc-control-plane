package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	sdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/sdkerrors"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
	"github.com/OpenRouterTeam/go-sdk/retry"
	aserr "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const openRouterReasoning = "openrouter.reasoning_details"

var errOpenRouter = errors.New("OpenRouter request failed or returned an incomplete response")

type openRouterChatModel struct {
	client    *sdk.OpenRouter
	name      string
	maxTokens int
}

func newOpenRouterChatModel(
	name, key, baseURL string,
	maxTokens int,
	client *http.Client,
) *openRouterChatModel {
	options := []sdk.SDKOption{
		sdk.WithClient(client),
		sdk.WithSecurity(key),
		sdk.WithRetryConfig(retry.Config{Strategy: "none"}),
	}
	if baseURL != "" {
		options = append(options, sdk.WithServerURL(baseURL))
	}
	return &openRouterChatModel{
		client:    sdk.New(options...),
		name:      name,
		maxTokens: maxTokens,
	}
}

func (m *openRouterChatModel) ModelName() string { return m.name }

func (m *openRouterChatModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return (&model.OpenAIChatModel{}).CountTokens(msgs, tools)
}

func (m *openRouterChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	request, err := m.request(msgs, false, opts)
	if err != nil {
		return nil, err
	}
	response, err := m.client.Chat.Send(ctx, request, nil)
	if response != nil && response.EventStream != nil {
		_ = response.EventStream.Close()
	}
	if err != nil || response == nil || response.ChatResult == nil {
		return nil, openRouterFailure(ctx, err)
	}
	result := response.ChatResult
	if len(result.Choices) != 1 || result.Choices[0].Index != 0 || result.Choices[0].FinishReason == nil {
		return nil, errOpenRouter
	}
	choice := result.Choices[0]
	if refused, _ := choice.Message.Refusal.GetOrZero(); refused != "" {
		return nil, errOpenRouter
	}
	content, _ := choice.Message.Content.GetOrZero()
	var text string
	if content.Str != nil {
		text = *content.Str
	}
	calls := make([]message.ToolCallBlock, 0, len(choice.Message.ToolCalls))
	for _, tool := range choice.Message.ToolCalls {
		calls = append(calls, message.ToolCallBlock{
			Type: "tool_call", ID: tool.ID, Name: tool.Function.Name, Input: tool.Function.Arguments,
		})
	}
	return openRouterReply(result.ID, result.Model, string(*choice.FinishReason), text, calls, choice.Message.ReasoningDetails, result.Usage)
}

func (m *openRouterChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	request, err := m.request(msgs, true, opts)
	if err != nil {
		return nil, err
	}
	end := &openRouterStreamEnd{}
	response, err := m.client.Chat.Send(context.WithValue(ctx, openRouterEndKey{}, end), request, nil)
	if err != nil || response == nil || response.EventStream == nil {
		return nil, openRouterFailure(ctx, err)
	}
	stream := response.EventStream
	out := make(chan model.ChatResponse)
	go func() {
		defer close(out)
		defer func() { _ = stream.Close() }()

		var text strings.Builder
		var details []components.ReasoningDetailUnion
		var id, name, stop string
		var usage *components.ChatUsage
		calls := make(map[int64]message.ToolCallBlock)
		for stream.Next() {
			chunk := stream.Value().GetData()
			if chunk.Error != nil || len(chunk.Choices) > 1 {
				sendModelChunk(ctx, out, model.ChatResponse{Error: errOpenRouter})
				return
			}
			id, name = chunk.ID, chunk.Model
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if refused, _ := choice.Delta.Refusal.GetOrZero(); choice.Index != 0 || refused != "" {
				sendModelChunk(ctx, out, model.ChatResponse{Error: errOpenRouter})
				return
			}
			if choice.FinishReason != nil {
				stop = string(*choice.FinishReason)
			}
			details = append(details, choice.Delta.ReasoningDetails...)
			for _, fragment := range choice.Delta.ToolCalls {
				if fragment.Index < 0 {
					sendModelChunk(ctx, out, model.ChatResponse{Error: errOpenRouter})
					return
				}
				call := calls[fragment.Index]
				call.Type = "tool_call"
				if fragment.ID != nil {
					if call.ID != "" && call.ID != *fragment.ID {
						sendModelChunk(ctx, out, model.ChatResponse{Error: errOpenRouter})
						return
					}
					call.ID = *fragment.ID
				}
				if fragment.Function != nil {
					if fragment.Function.Name != nil {
						call.Name += *fragment.Function.Name
					}
					if fragment.Function.Arguments != nil {
						call.Input += *fragment.Function.Arguments
					}
				}
				calls[fragment.Index] = call
			}
			if delta, _ := choice.Delta.Content.GetOrZero(); delta != "" {
				text.WriteString(delta)
				if !sendModelChunk(ctx, out, model.ChatResponse{
					ID: id, ModelName: name, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: delta}},
				}) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil || !end.complete() {
			sendModelChunk(ctx, out, model.ChatResponse{Error: openRouterFailure(ctx, err)})
			return
		}
		indices := make([]int64, 0, len(calls))
		for index := range calls {
			indices = append(indices, index)
		}
		slices.Sort(indices)
		ordered := make([]message.ToolCallBlock, 0, len(calls))
		for _, index := range indices {
			ordered = append(ordered, calls[index])
		}
		completed, err := openRouterReply(id, name, stop, text.String(), ordered, details, usage)
		if err != nil {
			sendModelChunk(ctx, out, model.ChatResponse{Error: err})
			return
		}
		sendModelChunk(ctx, out, *completed)
	}()
	return out, nil
}

func openRouterReply(id, name, stop, text string, calls []message.ToolCallBlock, details []components.ReasoningDetailUnion, usage *components.ChatUsage) (*model.ChatResponse, error) {
	if stop != model.StopReasonStop && stop != model.StopReasonToolCalls ||
		(len(calls) > 0) != (stop == model.StopReasonToolCalls) || text == "" && len(calls) == 0 {
		return nil, errOpenRouter
	}
	reply := &model.ChatResponse{ID: id, ModelName: name, StopReason: stop, IsLast: true}
	if len(details) > 0 {
		reply.Content = append(reply.Content, message.ThinkingBlock{
			Type: "thinking", Extra: map[string]any{openRouterReasoning: details},
		})
	}
	if text != "" {
		reply.Content = append(reply.Content, message.TextBlock{Type: "text", Text: text})
	}
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Name == "" || seen[call.ID] || !json.Valid([]byte(call.Input)) {
			return nil, errOpenRouter
		}
		seen[call.ID] = true
		reply.Content = append(reply.Content, call)
	}
	// Zero-valued SDK fields can be absent. Use a reported total to disambiguate.
	if usage != nil && usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 &&
		(usage.PromptTokens > 0 && usage.CompletionTokens > 0 ||
			usage.TotalTokens > 0 && usage.TotalTokens == usage.PromptTokens+usage.CompletionTokens) {
		input := int(usage.PromptTokens)
		reply.Usage = &model.ChatUsage{InputTokens: input, OutputTokens: int(usage.CompletionTokens)}
		if detail, ok := usage.PromptTokensDetails.GetOrZero(); ok {
			cached, created := 0, 0
			if detail.CachedTokens != nil {
				cached = int(*detail.CachedTokens)
			}
			if detail.CacheWriteTokens != nil {
				created = int(*detail.CacheWriteTokens)
			}
			if cached >= 0 && created >= 0 && cached <= input && created <= input-cached {
				reply.Usage.InputTokens = input - cached - created
				reply.Usage.TotalInputTokens = input
				reply.Usage.CacheInputTokens = cached
				reply.Usage.CacheCreationInputTokens = created
			}
		}
	}
	return reply, nil
}

func openRouterFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var invalid *sdkerrors.BadRequestResponseError
	if errors.As(err, &invalid) && model.IsContextLimitExceeded(errors.New(invalid.Error_.Message)) {
		return aserr.ErrModelContextLimit
	}
	switch err.(type) {
	case *sdkerrors.TooManyRequestsResponseError, *sdkerrors.BadGatewayResponseError,
		*sdkerrors.ServiceUnavailableResponseError, *sdkerrors.GatewayTimeoutResponseError,
		*sdkerrors.ProviderOverloadedResponseError, *sdkerrors.InternalServerResponseError:
		return aserr.NewThrottled(0, "AI provider temporarily unavailable")
	}
	if err != nil {
		return redactModelError(err)
	}
	return errOpenRouter
}

type openRouterEndKey struct{}

// The pinned SDK does not distinguish an exhausted reader from the SSE terminator.
type openRouterStreamEnd struct {
	io.ReadCloser
	line       [len("data: [DONE]")]byte
	lineLength int
	previousCR bool
	hasData    bool
	sentinel   bool
	done       bool
	failed     bool
}

func (s *openRouterStreamEnd) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.failed = s.failed || err != nil && err != io.EOF
	// Observe only the data field of a complete frame. The SDK decodes the stream.
	for _, b := range p[:n] {
		if s.done {
			break
		}
		if b == '\n' && s.previousCR {
			s.previousCR = false
			continue
		}
		s.previousCR = b == '\r'
		if b == '\r' || b == '\n' {
			s.finishLine()
		} else {
			if s.lineLength < len(s.line) {
				s.line[s.lineLength] = b
			}
			s.lineLength++
		}
	}
	return n, err
}

func (s *openRouterStreamEnd) finishLine() {
	if s.lineLength == 0 {
		s.done = s.sentinel
		s.hasData, s.sentinel = false, false
		return
	}
	line := s.line[:min(s.lineLength, len(s.line))]
	if bytes.Equal(line, []byte("data")) || bytes.HasPrefix(line, []byte("data:")) {
		s.sentinel = !s.hasData && s.lineLength == len(line) &&
			(bytes.Equal(line, []byte("data:[DONE]")) || bytes.Equal(line, []byte("data: [DONE]")))
		s.hasData = true
	}
	s.lineLength = 0
}

func (s *openRouterStreamEnd) complete() bool { return s.done && !s.failed }

func (m *openRouterChatModel) request(msgs []*message.Msg, streaming bool, opts []model.CallOption) (components.ChatRequest, error) {
	var parameters model.CallOptions
	for _, opt := range opts {
		opt(&parameters)
	}
	maxTokens := m.maxTokens
	if parameters.MaxTokens != nil {
		maxTokens = min(maxTokens, *parameters.MaxTokens)
	}
	request := components.ChatRequest{
		Model: &m.name, Stream: &streaming,
		MaxCompletionTokens: optionalnullable.From(new(int64(maxTokens))),
		Temperature:         optionalnullable.From(parameters.Temperature),
		TopP:                optionalnullable.From(parameters.TopP),
		Seed:                optionalnullable.From(parameters.Seed),
		Provider: optionalnullable.From(&components.ProviderPreferences{
			Zdr:               optionalnullable.From(new(true)),
			AllowFallbacks:    optionalnullable.From(new(false)),
			RequireParameters: optionalnullable.From(new(true)),
			DataCollection:    optionalnullable.From(new(components.DataCollectionDeny)),
			Sort:              optionalnullable.From(new(components.CreateProviderPreferencesSortProviderSort(components.ProviderSortPrice))),
		}),
	}
	if streaming {
		request.StreamOptions = optionalnullable.From(&components.ChatStreamOptions{IncludeUsage: new(true)})
	}
	if parameters.ThinkingEnable != nil && !*parameters.ThinkingEnable {
		request.Reasoning = &components.ChatRequestReasoning{Effort: optionalnullable.From(new(components.ChatRequestEffortNone))}
	} else if parameters.ReasoningEffort != nil {
		effort := components.ChatRequestEffort(*parameters.ReasoningEffort)
		if !effort.IsExact() {
			return components.ChatRequest{}, errOpenRouter
		}
		request.Reasoning = &components.ChatRequestReasoning{Effort: optionalnullable.From(&effort)}
	}

	var err error
	request.Messages, err = openRouterMessages(msgs)
	if err != nil {
		return components.ChatRequest{}, err
	}
	request.Tools, request.ToolChoice, err = openRouterTools(parameters.Tools, parameters.ToolChoice)
	if err != nil {
		return components.ChatRequest{}, err
	}
	request.ResponseFormat, err = openRouterFormat(parameters.ResponseFormat)
	return request, err
}

func openRouterMessages(msgs []*message.Msg) ([]components.ChatMessages, error) {
	native := formatter.NewOpenAIFormatter()
	var wire []map[string]any
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		blocks := msg.GetContentBlocks()
		start := 0
		for i := 0; i <= len(blocks); i++ {
			end := i
			if i < len(blocks) {
				if _, ok := blocks[i].(message.ToolResultBlock); !ok {
					continue
				}
				end++
			} else if start == i && start != 0 {
				break
			}
			// Keep each reasoning signature with the assistant frame preceding its result.
			part := *msg
			part.Content = blocks[start:end]
			formatted, err := native.Format([]*message.Msg{&part})
			if err != nil {
				return nil, errOpenRouter
			}
			if len(formatted) > 0 && formatted[0]["role"] == "assistant" {
				var details []any
				for _, block := range part.Content {
					if thought, ok := block.(message.ThinkingBlock); ok {
						switch values := thought.Extra[openRouterReasoning].(type) {
						case []components.ReasoningDetailUnion:
							for _, value := range values {
								details = append(details, value)
							}
						case []any:
							details = append(details, values...)
						}
					}
				}
				if len(details) > 0 {
					formatted[0]["reasoning_details"] = details
				}
			}
			wire = append(wire, formatted...)
			start = end
		}
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, errOpenRouter
	}
	var messages []components.ChatMessages
	if json.Unmarshal(data, &messages) != nil {
		return nil, errOpenRouter
	}
	return messages, nil
}

func openRouterTools(schemas []model.ToolSchema, choice *model.ToolChoice) ([]components.ChatFunctionTool, *components.ChatToolChoice, error) {
	if choice != nil && len(choice.Tools) > 0 {
		var allowed []model.ToolSchema
		for _, schema := range schemas {
			if slices.Contains(choice.Tools, schema.Function.Name) {
				allowed = append(allowed, schema)
			}
		}
		schemas = allowed
	}
	if model.ValidateToolChoice(choice, schemas) != nil {
		return nil, nil, errOpenRouter
	}
	var tools []components.ChatFunctionTool
	for _, schema := range schemas {
		var parameters map[string]any
		if json.Unmarshal(schema.Function.Parameters, &parameters) != nil {
			return nil, nil, errOpenRouter
		}
		tools = append(tools, components.CreateChatFunctionToolChatFunctionToolFunction(components.ChatFunctionToolFunction{
			Type: components.ChatFunctionToolTypeFunction,
			Function: components.ChatFunctionToolFunctionFunction{
				Name: schema.Function.Name, Description: &schema.Function.Description, Parameters: parameters,
			},
		}))
	}
	if choice == nil || choice.Mode == "" || choice.Mode == "auto" {
		return tools, nil, nil
	}
	switch choice.Mode {
	case "none":
		return tools, new(components.CreateChatToolChoiceChatToolChoiceNone(components.ChatToolChoiceNoneNone)), nil
	case "required":
		return tools, new(components.CreateChatToolChoiceChatToolChoiceRequired(components.ChatToolChoiceRequiredRequired)), nil
	default:
		return tools, new(components.CreateChatToolChoiceChatNamedToolChoice(components.ChatNamedToolChoice{
			Type:     components.ChatNamedToolChoiceTypeFunction,
			Function: components.ChatNamedToolChoiceFunction{Name: choice.Mode},
		})), nil
	}
}

func openRouterFormat(format *model.ResponseFormat) (*components.ResponseFormat, error) {
	if format == nil || format.Type == "text" {
		return nil, nil
	}
	switch format.Type {
	case "json_object":
		return new(components.CreateResponseFormatJSONObject(components.ChatFormatJSONObjectConfig{
			Type: components.ChatFormatJSONObjectConfigTypeJSONObject,
		})), nil
	case "json_schema":
		var schema map[string]any
		if json.Unmarshal(format.JSONSchema, &schema) != nil || schema == nil {
			return nil, errOpenRouter
		}
		name := format.Name
		if name == "" {
			name = "response"
		}
		return new(components.CreateResponseFormatJSONSchema(components.ChatFormatJSONSchemaConfig{
			Type: components.ChatFormatJSONSchemaConfigTypeJSONSchema,
			JSONSchema: components.ChatJSONSchemaConfig{
				Name: name, Schema: schema, Strict: optionalnullable.From(&format.Strict),
			},
		})), nil
	default:
		return nil, errOpenRouter
	}
}

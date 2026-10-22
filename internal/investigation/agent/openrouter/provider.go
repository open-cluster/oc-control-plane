package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	sdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/sdkerrors"
	"github.com/OpenRouterTeam/go-sdk/retry"

	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

const Name = "openrouter"

type Provider struct {
	client *sdk.OpenRouter
	config agent.ModelConfig
}

type Options struct {
	HTTPClient *http.Client
}

func New(config agent.ModelConfig, options Options) (*Provider, error) {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	client := http.Client{}
	if options.HTTPClient != nil {
		client = *options.HTTPClient
	}
	// Redirects must never move provider credentials or Investigation context to another host.
	// So when redirect is invoked we return an error and stop the redirect to be happened.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = boundedTransport{base: transport}
	settings := []sdk.SDKOption{
		sdk.WithSecurity(config.Credential.Reveal()), sdk.WithClient(&client),
		sdk.WithRetryConfig(retry.Config{
			Strategy:              "backoff", // either "backoff", "none" or ""
			RetryConnectionErrors: true,
			Backoff: &retry.BackoffStrategy{
				InitialInterval: 500,
				MaxInterval:     5_000,
				Exponent:        2,
				MaxElapsedTime:  30_000, // Stop retrying after roughly 30 seconds
			},
		}),
	}
	if config.BaseURL != "" {
		settings = append(settings, sdk.WithServerURL(config.BaseURL))
	}
	return &Provider{
		client: sdk.New(settings...),
		config: config,
	}, nil
}

func (p *Provider) EstimateRequestTokens(prompt agent.Prompt) (int, error) {
	body, err := request(prompt)
	if err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("encoding the model request for budgeting")
	}
	return agent.EstimateSerializedRequest(encoded), nil
}

func (p *Provider) Complete(ctx context.Context, prompt agent.Prompt) (agent.Completion, error) {
	requestBody, err := request(prompt)
	if err != nil {
		return agent.Completion{}, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "invalid model continuation")
	}

	ctx, cancel := context.WithTimeout(ctx, p.config.RequestTimeout)
	defer cancel()
	info := &responseInfo{}
	ctx = context.WithValue(ctx, responseKey{}, info)

	response, err := p.client.Chat.Send(ctx, requestBody, nil)
	completion := agent.Completion{
		Model:     prompt.Model,
		RequestID: info.identifier,
	}
	if err != nil {
		return completion, failure(ctx, prompt.Model, info.status, err)
	}
	if response == nil || response.ChatResult == nil {
		if response != nil && response.EventStream != nil {
			_ = response.EventStream.Close()
		}
		return completion, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "the provider returned no complete decision")
	}
	result := response.ChatResult
	completion.RequestID = result.ID
	completion.Usage = usageOf(result.Usage)
	if result.Model != "" {
		completion.Model = result.Model
	}
	if len(result.Choices) != 1 || result.Choices[0].FinishReason == nil {
		return completion, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "the provider returned no single complete decision")
	}
	choice := result.Choices[0]
	switch string(*choice.FinishReason) {
	case "tool_calls":
		completion.Stop = agent.StopToolUse
	case "stop":
		completion.Stop = agent.StopComplete
	case "length":
		completion.Stop = agent.StopTruncated
		return completion, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "the answer reached the output ceiling")
	case "content_filter":
		completion.Stop = agent.StopRefused
		return completion, agent.Failed(
			agent.OutcomeRefused, Name, prompt.Model, "the provider declined the request")
	default:
		return completion, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "the provider returned an unsupported stop reason")
	}
	message := choice.Message
	if refusal, _ := message.Refusal.GetOrZero(); refusal != "" {
		completion.Stop = agent.StopRefused
		return completion, agent.Failed(
			agent.OutcomeRefused, Name, prompt.Model, "the provider declined the request")
	}
	seen := make(map[string]bool, len(message.ToolCalls))

	for _, call := range message.ToolCalls {
		if call.ID == "" || seen[call.ID] || call.Function.Name == "" {
			return completion, agent.Failed(
				agent.OutcomeMalformed, Name, prompt.Model, "the provider returned an invalid Tool call")
		}
		seen[call.ID] = true
		name := call.Function.Name
		for _, definition := range prompt.Tools {
			if name == toolName(definition.Name) {
				name = definition.Name
				break
			}
		}
		completion.ToolCalls = append(completion.ToolCalls, agent.CompletionCall{
			ID:        call.ID,
			Name:      name,
			Arguments: json.RawMessage(call.Function.Arguments),
		})
	}
	if content, _ := message.Content.GetOrZero(); content.Str != nil {
		completion.Document = []byte(*content.Str)
	}
	completion.Raw, err = json.Marshal(message)
	if err != nil {
		return completion, agent.Failed(
			agent.OutcomeMalformed, Name, prompt.Model, "the model continuation could not be retained")
	}
	return completion, nil
}

func usageOf(usage *components.ChatUsage) agent.TokenUsage {
	if usage == nil {
		return agent.TokenUsage{}
	}

	result := agent.TokenUsage{
		Input:  agent.Counted(usage.PromptTokens),
		Output: agent.Counted(usage.CompletionTokens),
	}
	details, _ := usage.PromptTokensDetails.GetOrZero()
	if details.CachedTokens != nil {
		cached := min(max(*details.CachedTokens, 0), usage.PromptTokens)
		result.CacheRead = agent.Counted(cached)
		result.Input = agent.Counted(usage.PromptTokens - cached)
	}
	if details.CacheWriteTokens != nil {
		written := min(max(*details.CacheWriteTokens, 0), result.Input.Tokens)
		result.CacheWrite = agent.Counted(written)
		result.Input = agent.Counted(result.Input.Tokens - written)
	}
	if details, _ := usage.CompletionTokensDetails.GetOrZero(); details.ReasoningTokens.IsSet() && !details.ReasoningTokens.IsNull() {
		tokens, _ := details.ReasoningTokens.GetOrZero()
		result.Reasoning = agent.Counted(tokens)
	}
	return result
}

func failure(ctx context.Context, model string, status int, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		return agent.FailedBecause(
			agent.OutcomeTimeout, Name, model, "the model request was cancelled or timed out", ctx.Err())
	}
	var invalid *sdkerrors.BadRequestResponseError
	if errors.As(cause, &invalid) {
		text := strings.ToLower(invalid.Error_.Message)
		if strings.Contains(text, "context") && (strings.Contains(text, "length") || strings.Contains(text, "limit") || strings.Contains(text, "window")) {
			return agent.ContextRejected(
				Name, model, "the provider rejected the request context", nil)
		}
	}
	switch {
	case status == http.StatusOK:
		return agent.Failed(
			agent.OutcomeMalformed, Name, model, "the provider returned an invalid response")
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 || status == 0:
		return agent.Failed(
			agent.OutcomeOutage, Name, model, "the provider is unavailable or rate limited")
	default:
		return agent.Failed(
			agent.OutcomeRejected, Name, model, fmt.Sprintf("the provider answered %d; check the credential, model, Tool support and zero-retention availability", status))
	}
}

type responseKey struct{}

type responseInfo struct {
	status     int
	identifier string
}

type boundedTransport struct {
	base http.RoundTripper
}

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if response != nil {

		// ------ INFO ------
		// The context metadata will be received by responseInfo
		// ------ INFO ------
		if info, ok := request.Context().Value(responseKey{}).(*responseInfo); ok {
			info.status = response.StatusCode
			info.identifier = response.Header.Get("x-request-id")
		}
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(response.Body, 8<<20), response.Body}
	}
	return response, err
}

package ai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	aserr "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

const (
	modelRequestTimeout = 5 * time.Minute
	maxProviderBody     = 8 << 20
)

// NewChatModel assembles an AgentScope model from the deployment's AI configuration.
// Ollama uses the host profile's context size for both requests and compaction.
func NewChatModel(settings config.Config, baseURL string, contextSize int) (model.ChatModel, error) {
	name := strings.TrimSpace(settings.ModelName)
	key := strings.TrimSpace(settings.ModelAPIKey)
	provider := settings.ModelProvider
	if name == "" {
		return nil, errors.New("AI model identifier is required")
	}
	if key == "" && provider != "ollama" {
		return nil, errors.New("AI provider credential is required")
	}
	if baseURL != "" {
		endpoint, err := url.Parse(baseURL)
		if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.ForceQuery ||
			endpoint.RawQuery != "" || strings.Contains(baseURL, "#") || !modelEndpointAllowed(endpoint) {
			return nil, errors.New("AI provider endpoint must use HTTPS or local loopback")
		}
		if provider == "ollama" && key == "" && !modelLoopback(endpoint.Hostname()) {
			return nil, errors.New("a credential is required for a remote Ollama endpoint")
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	credential := model.NewSecretStr(key)
	client := &http.Client{
		Timeout:   modelRequestTimeout,
		Transport: modelTransport{redactStatus: provider != "openrouter"},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	maxTokens := int(settings.ModelMaxOutputTokens)
	if maxTokens <= 0 && (provider == "anthropic" || provider == "openrouter") {
		maxTokens = 32_000
	}

	var native model.ChatModel
	var err error
	switch provider {
	case "openrouter":
		return newOpenRouterChatModel(name, key, baseURL, maxTokens, client), nil
	case "anthropic":
		native, err = model.NewAnthropicChatModel(&model.AnthropicConfig{
			SecretAPIKey:    credential,
			BaseURL:         baseURL,
			Model:           name,
			MaxOutputTokens: maxTokens,
			HTTPClient:      client,
		})
	case "openai":
		native, err = model.NewOpenAIChatModel(model.OpenAIConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "gemini":
		native, err = model.NewGeminiChatModel(model.GeminiConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "deepseek":
		native, err = model.NewDeepSeekChatModel(model.DeepSeekConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "dashscope":
		native, err = model.NewDashScopeChatModel(model.DashScopeConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "moonshot", "kimi":
		native, err = model.NewMoonshotChatModel(model.MoonshotConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "xai":
		native, err = model.NewXAIChatModel(model.XAIConfig{
			SecretAPIKey: credential,
			BaseURL:      baseURL,
			Model:        name,
			HTTPClient:   client,
		})
	case "ollama":
		if contextSize <= 1 {
			return nil, errors.New("an explicit context capacity is required for Ollama")
		}
		if maxTokens >= contextSize {
			return nil, errors.New("configured Ollama output allowance must leave room for input in the context window")
		}
		ollama := model.OllamaNativeConfig{
			BaseURL:    baseURL,
			Model:      name,
			HTTPClient: client,
			NumCtx:     contextSize,
		}
		if key != "" {
			ollama.ClientOptions = &model.ClientOptions{
				DefaultHeaders: map[string]string{"Authorization": "Bearer " + key},
			}
		}
		native, err = model.NewOllamaNativeChatModel(ollama)
	default:
		return nil, errors.New("unsupported AI provider")
	}
	if err != nil {
		return nil, errors.New("AI provider configuration is invalid")
	}
	return deploymentModel{ChatModel: native, maxTokens: maxTokens}, nil
}

func modelEndpointAllowed(endpoint *url.URL) bool {
	return endpoint.Scheme == "https" || (endpoint.Scheme == "http" && modelLoopback(endpoint.Hostname()))
}

func modelLoopback(host string) bool {
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

type modelTransport struct {
	redactStatus bool
}

func (t modelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxProviderBody)
	if tracker, ok := request.Context().Value(openRouterEndKey{}).(*openRouterStreamEnd); ok &&
		strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		tracker.ReadCloser = response.Body
		response.Body = tracker
	}
	if t.redactStatus && (response.StatusCode < 200 || response.StatusCode >= 300) {
		var detail string
		if response.StatusCode == http.StatusBadRequest {
			body, readErr := io.ReadAll(response.Body)
			if readErr == nil && model.IsContextLimitExceeded(errors.New(string(body))) {
				detail = "context window exceeded"
			}
		}
		_ = response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(detail))
		response.ContentLength = int64(len(detail))
	}
	return response, nil
}

// deploymentModel keeps provider errors and limits safe without changing native message handling.
type deploymentModel struct {
	model.ChatModel
	maxTokens int
}

func (m deploymentModel) Unwrap() model.ChatModel { return m.ChatModel }

func (m deploymentModel) DisableThinkingOptions() []model.CallOption {
	if capable, ok := m.ChatModel.(model.ThinkingDisabler); ok {
		return capable.DisableThinkingOptions()
	}
	return nil
}

func (m deploymentModel) limitOutput(options *model.CallOptions) {
	if m.maxTokens > 0 {
		if options.MaxTokens == nil || *options.MaxTokens <= 0 || *options.MaxTokens > m.maxTokens {
			options.MaxTokens = &m.maxTokens
		}
		return
	}
	ollama, ok := m.ChatModel.(*model.OllamaNativeChatModel)
	if ok && options.MaxTokens != nil && *options.MaxTokens >= ollama.ContextSize() {
		limit := ollama.ContextSize() - 1
		options.MaxTokens = &limit
	}
}

func (m deploymentModel) Chat(ctx context.Context, history []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	options := append(slices.Clone(opts), m.limitOutput)
	switch m.ChatModel.(type) {
	case *model.AnthropicChatModel, *model.OpenAIChatModel:
		// These native models wrap the retrying HTTP helper with an optional second loop.
		options = append(options, model.WithRetries(0, 0))
	}
	result, err := m.ChatModel.Chat(ctx, history, options...)
	if err != nil {
		return nil, redactModelError(err)
	}
	if result != nil {
		result.Error = redactModelError(result.Error)
	}
	return result, nil
}

func (m deploymentModel) ChatStream(ctx context.Context, history []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	stream, err := m.ChatModel.ChatStream(ctx, history, append(slices.Clone(opts), m.limitOutput)...)
	if err != nil || stream == nil {
		return nil, redactModelError(err)
	}
	out := make(chan model.ChatResponse)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-stream:
				if !ok {
					return
				}
				chunk.Error = redactModelError(chunk.Error)
				if !sendModelChunk(ctx, out, chunk) {
					return
				}
			}
		}
	}()
	return out, nil
}

func redactModelError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, model.ErrStreamNotSupported):
		return model.ErrStreamNotSupported
	case errors.Is(err, aserr.ErrModelContextLimit), model.IsContextLimitExceeded(err):
		return aserr.ErrModelContextLimit
	case model.IsRetryableError(err):
		delay, _ := aserr.RetryAfterOf(err)
		return aserr.NewThrottled(delay, "AI provider temporarily unavailable")
	default:
		return errors.New("AI provider request failed")
	}
}

func sendModelChunk(ctx context.Context, out chan<- model.ChatResponse, chunk model.ChatResponse) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- chunk:
		return true
	}
}

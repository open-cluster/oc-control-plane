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
func NewChatModel(settings config.Config, baseURL string) (model.ChatModel, error) {
	name := strings.TrimSpace(settings.ModelName)
	if name == "" || strings.TrimSpace(settings.ModelAPIKey) == "" {
		return nil, errors.New("AI model identifier and credential are required")
	}
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
			parsed.Fragment != "" || !modelEndpointAllowed(parsed) {
			return nil, errors.New("AI provider endpoint must use HTTPS or local loopback")
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{
		Timeout:   modelRequestTimeout,
		Transport: modelTransport{redactStatus: settings.ModelProvider != "openrouter"},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	maxTokens := int(settings.ModelMaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = 32_000
	}

	switch settings.ModelProvider {
	case "openrouter":
		return newOpenRouter(name, settings.ModelAPIKey, baseURL, maxTokens, client), nil
	case "anthropic":
		native, err := model.NewAnthropicChatModel(&model.AnthropicConfig{
			SecretAPIKey:    model.NewSecretStr(settings.ModelAPIKey),
			BaseURL:         baseURL,
			Model:           name,
			MaxOutputTokens: maxTokens,
			HTTPClient:      client,
		})
		if err != nil {
			return nil, errors.New("AI provider configuration is invalid")
		}
		return deploymentModel{ChatModel: native, maxTokens: maxTokens}, nil
	default:
		return nil, errors.New("unsupported AI provider")
	}
}

func modelEndpointAllowed(endpoint *url.URL) bool {
	if endpoint.Scheme == "https" {
		return true
	}
	host := endpoint.Hostname()
	return endpoint.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback())
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

func (m deploymentModel) limits(options *model.CallOptions) {
	if options.MaxTokens == nil || *options.MaxTokens > m.maxTokens {
		options.MaxTokens = &m.maxTokens
	}
	// Anthropic's buffered HTTP helper retries. Disable the optional model-level loop.
	options.MaxRetries = 0
}

func (m deploymentModel) Chat(ctx context.Context, history []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	result, err := m.ChatModel.Chat(ctx, history, append(slices.Clone(opts), m.limits)...)
	if err != nil {
		return nil, redactModelError(err)
	}
	if result != nil {
		result.Error = redactModelError(result.Error)
	}
	return result, nil
}

func (m deploymentModel) ChatStream(ctx context.Context, history []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	stream, err := m.ChatModel.ChatStream(ctx, history, append(slices.Clone(opts), m.limits)...)
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

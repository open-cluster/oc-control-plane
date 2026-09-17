package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Name is how this provider is written in configuration and telemetry.
const Name = "zai"

// defaultBaseURL is where Z.AI serves its open platform API.
const defaultBaseURL = "https://api.z.ai"

// completionsPath is the chat completions endpoint, relative to the base.
const completionsPath = "/api/paas/v4/chat/completions"

const maxResponseBytes = 8 << 20

// Provider is one configured Z.AI model.
type Provider struct {
	client   *http.Client
	endpoint string
	config   reasoning.ModelConfig
	wait     func(context.Context, time.Duration) error
}

func usageOf(reported usage) reasoning.TokenUsage {
	normalized := reasoning.TokenUsage{
		Input:      reasoning.Counted(reported.PromptTokens),
		Output:     reasoning.Counted(reported.CompletionTokens),
		CacheWrite: reasoning.Unreported(),
		CacheRead:  reasoning.Unreported(),
		Reasoning:  reasoning.Unreported(),
	}
	if details := reported.PromptTokensDetails; details != nil {
		cached := details.CachedTokens
		if cached > reported.PromptTokens {
			cached = reported.PromptTokens
		}
		normalized.CacheRead = reasoning.Counted(cached)
		normalized.Input = reasoning.Counted(reported.PromptTokens - cached)
	}
	if details := reported.CompletionTokensDetails; details != nil {
		normalized.Reasoning = reasoning.Counted(details.ReasoningTokens)
	}
	return normalized
}

func stopOf(reason string) reasoning.Stop {
	switch reason {
	case "sensitive":
		return reasoning.StopRefused
	case "length", "model_context_window_exceeded":
		return reasoning.StopTruncated
	case "tool_calls":
		return reasoning.StopToolUse
	default:
		return reasoning.StopComplete
	}
}

func classify(model string, status int, identifier string, payload []byte) error {
	detail := fmt.Sprintf("the provider answered %d", status)
	if identifier != "" {
		detail += " (request " + identifier + ")"
	}
	message := errorMessage(payload)
	if message != "" {
		detail += ": " + message
	}

	switch {
	case status == http.StatusBadRequest && isContextLimitError(message):
		return reasoning.ContextRejected(Name, model, detail, nil)
	case status == http.StatusRequestTimeout || status == http.StatusConflict ||
		status == http.StatusTooManyRequests:
		return reasoning.Failed(reasoning.OutcomeOutage, Name, model, detail+": rate limited")
	case status >= 500:
		return reasoning.Failed(reasoning.OutcomeOutage, Name, model,
			detail+": the provider failed on its own side")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return reasoning.Failed(reasoning.OutcomeRejected, Name, model,
			detail+": the credential was not accepted")
	case status == http.StatusNotFound:
		return reasoning.Failed(reasoning.OutcomeRejected, Name, model,
			detail+": the model identifier is not one this provider serves")
	case status >= 400:
		return reasoning.Failed(reasoning.OutcomeRejected, Name, model, detail)
	default:
		return reasoning.Failed(reasoning.OutcomeOutage, Name, model, detail)
	}
}

func isContextLimitError(detail string) bool {
	normalized := strings.ToLower(detail)
	return strings.Contains(normalized, "context_length_exceeded") ||
		strings.Contains(normalized, "context window") ||
		strings.Contains(normalized, "context length")
}

func errorMessage(payload []byte) string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	switch {
	case envelope.Error.Message != "" && envelope.Error.Code != "":
		return envelope.Error.Code + " " + envelope.Error.Message
	case envelope.Error.Message != "":
		return envelope.Error.Message
	default:
		return envelope.Error.Code
	}
}

func transportFailure(model string, cause error) error {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, Name, model,
			"the round's deadline was reached before the provider answered", cause)
	case errors.Is(cause, context.Canceled):
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, Name, model,
			"the investigation was cancelled while waiting for the provider", cause)
	}

	var timeout net.Error
	if errors.As(cause, &timeout) && timeout.Timeout() {
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, Name, model,
			"the provider did not answer within the configured request timeout", cause)
	}
	return reasoning.FailedBecause(reasoning.OutcomeOutage, Name, model,
		"the provider could not be reached", cause)
}

// Options is what a caller may put in place of the real thing for an offline test.
type Options struct {
	HTTPClient *http.Client
	Wait       func(context.Context, time.Duration) error
}

// New builds a provider for one model configuration, refusing a configuration that could not work.
func New(config reasoning.ModelConfig, options Options) (*Provider, error) {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	base := config.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: config.RequestTimeout,
			// A redirect is refused rather than followed. The host this adapter may reach comes
			// from configuration, and following a redirect would let a response decide where the
			// credential is sent next.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	wait := options.Wait
	if wait == nil {
		wait = waitForRetry
	}
	return &Provider{
		client:   client,
		endpoint: strings.TrimSuffix(base, "/") + completionsPath,
		config:   config,
		wait:     wait,
	}, nil
}

const (
	maxAttempts   = 3
	retryBase     = 500 * time.Millisecond
	retryDelayCap = 30 * time.Second
)

// RequestTokens sizes the same provider request structure Complete sends.
func (p *Provider) RequestTokens(prompt reasoning.Prompt) (int, error) {
	encoded, err := json.Marshal(p.request(prompt))
	if err != nil {
		return 0, err
	}
	return reasoning.EstimateSerializedRequest(encoded), nil
}

func (p *Provider) Complete(
	ctx context.Context, prompt reasoning.Prompt,
) (reasoning.Completion, error) {
	// Checked before anything is encoded or sent. A round whose deadline has already passed must
	// not continue an answer nobody is waiting for, and the transport cannot be relied on to
	// notice: it is asked to send a request, not to decide whether one is still wanted.
	if err := ctx.Err(); err != nil {
		return reasoning.Completion{}, transportFailure(prompt.Model, err)
	}

	body, err := json.Marshal(p.request(prompt))
	if err != nil {
		return reasoning.Completion{}, reasoning.Failed(reasoning.OutcomeRejected,
			Name, prompt.Model, "the request could not be encoded: "+err.Error())
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		completion, instructedDelay, err := p.once(ctx, prompt, body)
		if err == nil {
			return completion, nil
		}
		lastErr = err
		if !errors.Is(err, reasoning.ErrOutage) || ctx.Err() != nil || attempt == maxAttempts-1 {
			return completion, err
		}
		delay := instructedDelay
		if delay < 0 {
			delay = retryDelay(attempt)
		}
		if err := p.wait(ctx, delay); err != nil {
			return reasoning.Completion{}, transportFailure(prompt.Model, err)
		}
	}
	return reasoning.Completion{}, lastErr
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryDelay(attempt int) time.Duration {
	ceiling := retryBase << attempt
	if ceiling > retryDelayCap {
		ceiling = retryDelayCap
	}
	floor := ceiling / 2
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

// once performs one attempt.
func (p *Provider) once(
	ctx context.Context, prompt reasoning.Prompt, body []byte,
) (reasoning.Completion, time.Duration, error) {
	response, err := p.send(ctx, body)
	if err != nil {
		return reasoning.Completion{}, -1, transportFailure(prompt.Model, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return reasoning.Completion{}, -1, transportFailure(prompt.Model, err)
	}
	if response.StatusCode != http.StatusOK {
		return reasoning.Completion{}, retryAfter(response.Header.Get("Retry-After"), time.Now()), classify(
			prompt.Model, response.StatusCode, requestIdentifier(response, payload), payload)
	}
	completion, err := p.answer(prompt, response, payload)
	return completion, -1, err
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return -1
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(retryDelayCap/time.Second) {
			return retryDelayCap
		}
		return time.Duration(seconds) * time.Second
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return -1
	}
	return min(max(at.Sub(now), 0), retryDelayCap)
}

// send performs the request. The credential travels in a header and appears nowhere else.
func (p *Provider) send(ctx context.Context, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+p.config.Credential.Reveal())
	return p.client.Do(request)
}

// answer turns a successful body into a completion.
func (p *Provider) answer(
	prompt reasoning.Prompt, response *http.Response, payload []byte,
) (reasoning.Completion, error) {
	var decoded completionResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return reasoning.Completion{}, reasoning.Failed(reasoning.OutcomeMalformed,
			Name, prompt.Model, "the provider's own response envelope did not parse")
	}
	if len(decoded.Choices) == 0 {
		return reasoning.Completion{}, reasoning.Failed(reasoning.OutcomeMalformed,
			Name, prompt.Model, "the provider returned no choices")
	}

	choice := decoded.Choices[0]
	completion := reasoning.Completion{
		Model:     answeringModel(decoded.Model, prompt.Model),
		RequestID: firstNonEmpty(decoded.RequestID, decoded.ID, response.Header.Get("X-Request-Id")),
		Stop:      stopOf(choice.FinishReason),
		Usage:     usageOf(decoded.Usage),
	}

	// The finish reason is read BEFORE the content is, for the same reason it is on every other
	// provider: a declined request comes back as a successful response, and reading the content
	// first would present an empty one as a conclusion.
	switch completion.Stop {
	case reasoning.StopRefused:
		failure := reasoning.Failed(reasoning.OutcomeRefused, Name, completion.Model,
			"the provider's own safeguards declined this request")
		failure.Category = choice.FinishReason
		return completion, failure
	case reasoning.StopTruncated:
		return completion, reasoning.Failed(reasoning.OutcomeMalformed, Name, completion.Model,
			"the answer reached the output ceiling before it finished, so the document is "+
				"incomplete")
	}

	completion.Document = []byte(choice.Message.Content)
	for _, call := range choice.Message.ToolCalls {
		completion.ToolCalls = append(completion.ToolCalls, reasoning.CompletionCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: []byte(call.Function.Arguments),
		})
	}
	return completion, nil
}

// answeringModel reads which model actually replied, falling back to what was asked for only when
// the response does not say.
func answeringModel(answered, requested string) string {
	if trimmed := strings.TrimSpace(answered); trimmed != "" {
		return trimmed
	}
	return requested
}

// requestIdentifier digs the provider's own identifier out of a failed response, so a support
// conversation about a failure has the same handle as one about a success.
func requestIdentifier(response *http.Response, payload []byte) string {
	var envelope struct {
		RequestID string `json:"request_id"`
		ID        string `json:"id"`
	}
	_ = json.Unmarshal(payload, &envelope)
	return firstNonEmpty(envelope.RequestID, envelope.ID, response.Header.Get("X-Request-Id"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// completionResponse is this vendor's answer envelope, named exactly as it arrives.
type completionResponse struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	Choices   []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// ReasoningContent is this vendor's thinking output. It is read only so that it is
			// never mistaken for the document; nothing here records or logs it.
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

// usage is this vendor's token accounting.
type usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

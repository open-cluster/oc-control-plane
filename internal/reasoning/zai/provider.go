// Package zai reaches Z.AI's chat completions API for the GLM models, and is the only place in
// this program that knows Z.AI exists.
//
// It is hand-written against Z.AI's own documented API rather than borrowed from another vendor's
// client. Z.AI publishes an Anthropic-compatible endpoint, and pointing the Anthropic SDK at it
// would have been fewer lines — and would have made every record this system writes ambiguous
// about who actually answered, while quietly coupling this adapter to a translation layer neither
// vendor promises to keep stable. A provider is worth its own wire code.
package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Name is how this provider is written in configuration and telemetry.
const Name = "zai"

// defaultBaseURL is where Z.AI serves its open platform API.
const defaultBaseURL = "https://api.z.ai"

// completionsPath is the chat completions endpoint, relative to the base.
const completionsPath = "/api/paas/v4/chat/completions"

// maxResponseBytes bounds what this adapter will read from a response. A provider is not trusted
// to bound its own body: an unbounded read is how a misbehaving endpoint becomes this process's
// memory problem.
const maxResponseBytes = 8 << 20

// Provider is one configured Z.AI deployment.
type Provider struct {
	client     *http.Client
	endpoint   string
	deployment reasoning.Deployment
}

// Options is what a caller may put in place of the real thing. There is exactly one entry and it
// is the HTTP round-tripper, which is the seam every test in this package uses.
type Options struct {
	HTTPClient *http.Client
}

// New builds a provider for one deployment, refusing a configuration that could not work.
func New(deployment reasoning.Deployment, options Options) (*Provider, error) {
	deployment = deployment.WithDefaults()
	if err := deployment.Validate(); err != nil {
		return nil, err
	}

	base := deployment.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: deployment.RequestTimeout,
			// A redirect is refused rather than followed. The host this adapter may reach comes
			// from configuration, and following a redirect would let a response decide where the
			// credential is sent next.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Provider{
		client:     client,
		endpoint:   strings.TrimSuffix(base, "/") + completionsPath,
		deployment: deployment,
	}, nil
}

// Name identifies this vendor.
func (p *Provider) Name() string { return Name }

// Complete asks for one document.
//
// The call is deliberately non-streamed: this vendor reports token usage in one body on a
// non-streaming call, while on a streamed call the figures arrive in a final chunk this adapter
// would have to assume was sent. A cost figure that is silently absent is worse than a slower
// call. And because this vendor offers a JSON output MODE rather than schema enforcement, the
// schema is rendered into the prompt and the answer is validated by the caller — the guarantee
// survives, it just costs a retry instead of being free.
func (p *Provider) Complete(
	ctx context.Context, prompt reasoning.Prompt,
) (reasoning.Completion, error) {
	// Checked before anything is encoded or sent. A round whose deadline has already passed must
	// not spend money on an answer nobody is waiting for, and the transport cannot be relied on to
	// notice: it is asked to send a request, not to decide whether one is still wanted.
	if err := ctx.Err(); err != nil {
		return reasoning.Completion{}, transportFailure(prompt.Model, err)
	}

	body, err := json.Marshal(p.request(prompt))
	if err != nil {
		return reasoning.Completion{}, reasoning.Failed(reasoning.OutcomeRejected,
			Name, prompt.Model, "the request could not be encoded: "+err.Error())
	}

	response, err := p.send(ctx, body)
	if err != nil {
		return reasoning.Completion{}, transportFailure(prompt.Model, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return reasoning.Completion{}, transportFailure(prompt.Model, err)
	}
	if response.StatusCode != http.StatusOK {
		return reasoning.Completion{}, classify(
			prompt.Model, response.StatusCode, requestIdentifier(response, payload), payload)
	}
	return p.answer(prompt, response, payload)
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
	request.Header.Set("Authorization", "Bearer "+p.deployment.Credential.Reveal())
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
			ReasoningContent string `json:"reasoning_content"`
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

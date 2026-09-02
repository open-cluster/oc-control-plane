// Package anthropic reaches Anthropic's Messages API, and is the only place in this program that
// knows Anthropic exists.
//
// Everything vendor-shaped stops here: the SDK types, the wire vocabulary, the stop-reason
// spellings, the usage shape and the beta surface. What leaves this package is the same small set
// of types every other adapter returns, so a second vendor changes nothing above it.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Name is how this provider is written in configuration and telemetry.
const Name = "anthropic"

// Provider is one configured Anthropic deployment.
type Provider struct {
	client     sdk.Client
	deployment reasoning.Deployment
}

// Options is what a caller may put in place of the real thing. There is exactly one entry and it
// is the HTTP round-tripper, which is the seam every test in this package uses: the suite never
// reaches the network, because a test that called the real API would be non-deterministic, priced
// and offline-hostile.
type Options struct {
	HTTPClient *http.Client
}

// New builds a provider for one deployment, refusing a configuration that could not work.
//
// The base URL is also the only host this provider may reach. It is taken from configuration
// rather than from anything a response contains, so a redirect cannot move where the credential is
// sent.
func New(deployment reasoning.Deployment, options Options) (*Provider, error) {
	deployment = deployment.WithDefaults()
	if err := deployment.Validate(); err != nil {
		return nil, err
	}

	requestOptions := []option.RequestOption{
		option.WithAPIKey(deployment.Credential.Reveal()),
		// One attempt plus the retries that make up the rest. Retrying is what turns a rate limit
		// into an answer rather than an outage, and bounding it is what keeps the wall clock a
		// single call can consume inside the round's deadline.
		option.WithMaxRetries(deployment.MaxAttempts - 1),
		option.WithRequestTimeout(deployment.RequestTimeout),
	}
	if deployment.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(deployment.BaseURL))
	}
	// A redirect is refused rather than followed, on the client this adapter actually uses. The
	// host it may reach comes from configuration, and following a redirect would let a response
	// decide where the credential is sent next — which is the one thing the egress rule exists to
	// prevent. A test supplying its own client owns that decision itself.
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	requestOptions = append(requestOptions, option.WithHTTPClient(client))
	return &Provider{client: sdk.NewClient(requestOptions...), deployment: deployment}, nil
}

// Name identifies this vendor.
// Complete asks for one document.
//
// Every request streams. The output ceiling has to be generous because thinking and answer text
// share it on this model, and a large ceiling on a non-streaming request risks a transport timeout
// long before the model is finished — so the two decisions are one decision.
func (p *Provider) Complete(
	ctx context.Context, prompt reasoning.Prompt,
) (reasoning.Completion, error) {
	captured := &requestID{}
	stream := p.client.Messages.NewStreaming(ctx, p.params(prompt), captured.option())

	message := sdk.Message{}
	var streamErr error
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		streamErr = stream.Err()
	}

	// Built before the error is returned, not instead of it. A stream that failed partway still
	// consumed whatever it had already reported, and returning an empty completion here would
	// under-report the spend of exactly the calls most likely to be retried.
	completion := reasoning.Completion{
		Model:     answeringModel(message, prompt.Model),
		RequestID: captured.value(),
		Stop:      stopOf(message.StopReason),
		Usage:     usageOf(message.Usage),
	}
	if streamErr != nil {
		return completion, p.failure(prompt, captured.value(), streamErr)
	}

	// The stop reason is read BEFORE the content is. Reading the content first is the defect that
	// presents an empty or partial response as a conclusion.
	switch completion.Stop {
	case reasoning.StopRefused:
		return completion, refused(p.deployment.Provider, completion.Model, message.StopDetails)
	case reasoning.StopTruncated:
		return completion, reasoning.Failed(reasoning.OutcomeMalformed,
			p.deployment.Provider, completion.Model,
			"the answer reached the output ceiling before it finished, so the document is "+
				"incomplete")
	}

	completion.Document = []byte(textOf(message))
	completion.ToolCalls = toolCallsOf(message)
	// The whole turn is captured for verbatim replay: this vendor requires its own
	// thinking blocks, signatures included, echoed back during a tool loop.
	if raw, err := json.Marshal(message); err == nil {
		completion.Raw = raw
	}
	return completion, nil
}

// toolCallsOf reads the native calls out of the answer, in this system's shape.
func toolCallsOf(message sdk.Message) []reasoning.CompletionCall {
	var calls []reasoning.CompletionCall
	for _, block := range message.Content {
		if use, isUse := block.AsAny().(sdk.ToolUseBlock); isUse {
			calls = append(calls, reasoning.CompletionCall{
				ID: use.ID, Name: internalToolName(use.Name), Arguments: use.Input,
			})
		}
	}
	return calls
}

// answeringModel reads which model actually replied, falling back to what was asked for only when
// the response does not say: a provider may re-serve a request on another model, and the record
// must name what actually spoke.
func answeringModel(message sdk.Message, requested string) string {
	if answered := strings.TrimSpace(string(message.Model)); answered != "" {
		return answered
	}
	return requested
}

// textOf collects the answer text. Thinking blocks are skipped rather than concatenated: they are
// not the document, and a provider configured to summarise them would otherwise prepend prose to
// the JSON and fail the decode for a reason nobody could see.
func textOf(message sdk.Message) string {
	answer := &strings.Builder{}
	for _, block := range message.Content {
		if text, isText := block.AsAny().(sdk.TextBlock); isText {
			answer.WriteString(text.Text)
		}
	}
	return answer.String()
}

// requestID captures the provider's own identifier for a call, which is what a vendor support
// conversation is conducted in.
//
// It is per call rather than per client: one provider serves many concurrent rounds, and a field on
// the provider would report whichever request finished last.
type requestID struct {
	mutex      sync.Mutex
	identifier string
}

func (r *requestID) option() option.RequestOption {
	return option.WithMiddleware(func(
		request *http.Request, next option.MiddlewareNext,
	) (*http.Response, error) {
		response, err := next(request)
		if response != nil {
			r.mutex.Lock()
			if found := response.Header.Get("request-id"); found != "" {
				r.identifier = found
			}
			r.mutex.Unlock()
		}
		return response, err
	})
}

func (r *requestID) value() string {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.identifier
}

// failure turns whatever went wrong into one of this system's named outcomes.
func (p *Provider) failure(prompt reasoning.Prompt, identifier string, err error) error {
	var apiError *sdk.Error
	if errors.As(err, &apiError) {
		if identifier == "" {
			identifier = apiError.RequestID
		}
		return classify(p.deployment.Provider, prompt.Model, apiError.StatusCode, identifier, err)
	}
	return transportFailure(p.deployment.Provider, prompt.Model, err)
}

package zai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Normalizing this vendor's usage, finish reasons and failures into the shared vocabulary.
//
// The honest part of this file is what it does NOT claim. This vendor reports how many input
// tokens were served from cache and says nothing at all about tokens written to one. The write
// count is therefore recorded as absent rather than as zero — a figure of zero would say a cache
// write was measured and found to be nothing, which is a claim nobody made, and it would make a
// cache that stopped working indistinguishable from a vendor that never reported one.

// usageOf turns the vendor's usage object into the normalized shape.
//
// Cached tokens are reported as part of the prompt total on this vendor, so the plain input figure
// is the prompt total with the cached part taken out. Leaving them in would charge the cached
// portion at the full input rate, which is the exact mistake four-rate pricing exists to prevent.
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
			// A cached figure larger than the total it is part of is not usable. Trusting it
			// would produce a negative input count and a cost below zero.
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

// stopOf maps the vendor's finish reasons onto this system's.
//
// "sensitive" is this vendor's word for its own safeguards declining, and mapping it to a refusal
// is what keeps a declined request from being read as a short answer.
func stopOf(reason string) reasoning.Stop {
	switch reason {
	case "sensitive":
		return reasoning.StopRefused
	case "length", "model_context_window_exceeded":
		return reasoning.StopTruncated
	default:
		return reasoning.StopComplete
	}
}

// classify turns a failed status into a named outcome, keeping an outage distinguishable from a
// defect in this build.
func classify(model string, status int, identifier string, payload []byte) error {
	detail := fmt.Sprintf("the provider answered %d", status)
	if identifier != "" {
		detail += " (request " + identifier + ")"
	}
	if message := errorMessage(payload); message != "" {
		detail += ": " + message
	}

	switch {
	case status == http.StatusTooManyRequests:
		return reasoning.Failed(reasoning.OutcomeOutage, Name, model,
			detail+": rate limited")
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

// errorMessage reads what the provider said went wrong, and nothing else. The body is not echoed
// wholesale: it is a vendor's response and this text ends up in a log line.
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

// transportFailure names what went wrong before a status existed. A cancelled context is a timeout
// rather than an outage: the round's own deadline stopped the call, and reporting the vendor as
// unreachable would send someone to look at a service that was answering.
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
			"the provider did not answer within this deployment's request timeout", cause)
	}
	return reasoning.FailedBecause(reasoning.OutcomeOutage, Name, model,
		"the provider could not be reached", cause)
}

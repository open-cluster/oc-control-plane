package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Normalizing this vendor's usage and stop reasons into the shared vocabulary.
//
// This provider reports all four token figures, which is what makes cache effectiveness measurable
// rather than assumed: a cache that silently stopped working looks exactly like one that is
// working, unless both the write and the read counts are recorded.

// usageOf turns the vendor's usage object into the normalized shape.
//
// Input, output, cache write and cache read are always present on this API, so they are recorded as
// measured. Reasoning tokens are a newer decomposition and may be absent, so an absent figure stays
// absent rather than becoming a zero nobody measured.
func usageOf(usage sdk.Usage) reasoning.TokenUsage {
	normalized := reasoning.TokenUsage{
		Input:      reasoning.Counted(usage.InputTokens),
		Output:     reasoning.Counted(usage.OutputTokens),
		CacheWrite: reasoning.Counted(usage.CacheCreationInputTokens),
		CacheRead:  reasoning.Counted(usage.CacheReadInputTokens),
		Reasoning:  reasoning.Unreported(),
	}
	// Reasoning tokens sit INSIDE the output total on this API. They are recorded as a
	// decomposition for observability and must never be added to it, or every round that thought
	// would be billed twice in this system's own figures.
	if usage.OutputTokensDetails.JSON.ThinkingTokens.Valid() {
		normalized.Reasoning = reasoning.Counted(usage.OutputTokensDetails.ThinkingTokens)
	} else if usage.OutputTokensDetails.ThinkingTokens > 0 {
		normalized.Reasoning = reasoning.Counted(usage.OutputTokensDetails.ThinkingTokens)
	}
	return normalized
}

// stopOf maps the vendor's stop reasons onto this system's.
//
// The one that matters is refusal. It arrives as a SUCCESSFUL response carrying a refusal stop
// reason rather than as an error, so code that reads the content without checking this first would
// present an empty or partial response as a conclusion.
func stopOf(reason sdk.StopReason) reasoning.Stop {
	switch reason {
	case sdk.StopReasonRefusal:
		return reasoning.StopRefused
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		return reasoning.StopTruncated
	case sdk.StopReasonToolUse:
		return reasoning.StopToolUse
	default:
		return reasoning.StopComplete
	}
}

// refused turns a refusal into the named outcome, carrying the vendor's own category.
//
// A refusal is a failed round and never an abstention. An abstention is a finding about the
// evidence; a refusal is a fact about the provider, and reporting the first when the second is true
// would tell an engineer no explanation was supported when nobody ever looked.
func refused(provider, model string, details sdk.RefusalStopDetails) error {
	failure := reasoning.Failed(reasoning.OutcomeRefused, provider, model,
		"the provider's own safeguards declined this request")
	failure.Category = string(details.Category)
	if explanation := details.Explanation; explanation != "" {
		failure.Detail = explanation
	}
	return failure
}

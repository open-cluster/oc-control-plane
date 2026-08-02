package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Turning a provider-neutral prompt into this vendor's request.
//
// Three decisions are load-bearing and each of them is about this model specifically. Thinking is
// adaptive and its depth is set by effort rather than a token budget — the fixed-budget parameter
// is removed on this model and sending it is refused outright. Sampling parameters are removed too,
// so steering happens in the prompt and nowhere else. The answer is constrained by a declared JSON
// schema rather than parsed out of prose, which is why there are no tool definitions here at all.

// params builds the request for one prompt.
func (p *Provider) params(prompt reasoning.Prompt) sdk.MessageNewParams {
	return sdk.MessageNewParams{
		Model:     sdk.Model(prompt.Model),
		MaxTokens: prompt.MaxOutputTokens,
		System:    systemBlocks(prompt),
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(contentBlocks(prompt)...)},
		// Thinking is on by default on this model; asking for it explicitly says so in the code
		// rather than leaving the next reader to know it. Display stays omitted because nothing
		// here reads the reasoning: the document is the answer, and the round records the
		// reasoning token count rather than the reasoning.
		Thinking: sdk.ThinkingConfigParamUnion{
			OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: sdk.OutputConfigParam{
			Effort: effortOf(prompt.Effort),
			Format: sdk.JSONOutputFormatParam{Schema: prompt.Schema.Document},
		},
	}
}

// systemBlocks renders the frozen preamble, carrying the cache breakpoint where the prompt put it.
//
// The preamble is identical across every investigation in every organization, so it caches once and
// is read by everything. The minimum cacheable prefix on this model is 512 tokens; a shorter one
// does not cache and does not say so, which is why the preamble is written as one substantial block
// rather than several small ones.
func systemBlocks(prompt reasoning.Prompt) []sdk.TextBlockParam {
	blocks := make([]sdk.TextBlockParam, 0, len(prompt.System))
	for _, block := range prompt.System {
		rendered := sdk.TextBlockParam{Text: block.Text}
		if block.Cache {
			rendered.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		blocks = append(blocks, rendered)
	}
	return blocks
}

// contentBlocks renders the deliberation, carrying the second breakpoint at the end of the brief.
//
// Ordering is the whole game. Nothing volatile may appear before the last cached block, because
// caching is a prefix match and a byte that moves anywhere in the prefix invalidates everything
// after it — silently, and at full price.
func contentBlocks(prompt reasoning.Prompt) []sdk.ContentBlockParamUnion {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(prompt.Content))
	for _, block := range prompt.Content {
		rendered := &sdk.TextBlockParam{Text: block.Text}
		if block.Cache {
			rendered.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		blocks = append(blocks, sdk.ContentBlockParamUnion{OfText: rendered})
	}
	return blocks
}

// effortOf maps this system's effort vocabulary onto the vendor's. They happen to agree on this
// model, and the table exists so that a vendor that stops agreeing is a change here rather than a
// rejected request.
func effortOf(effort reasoning.Effort) sdk.OutputConfigEffort {
	switch effort {
	case reasoning.EffortLow:
		return sdk.OutputConfigEffortLow
	case reasoning.EffortMedium:
		return sdk.OutputConfigEffortMedium
	case reasoning.EffortExtraHigh:
		return sdk.OutputConfigEffortXhigh
	case reasoning.EffortMax:
		return sdk.OutputConfigEffortMax
	default:
		return sdk.OutputConfigEffortHigh
	}
}

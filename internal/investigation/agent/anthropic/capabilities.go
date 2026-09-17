package anthropic

import reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"

var modelCapabilities = map[string]reasoning.ModelCapabilities{
	"claude-sonnet-5":           {ContextWindowTokens: 1_000_000, MaxOutputTokens: 128_000},
	"claude-opus-5":             {ContextWindowTokens: 1_000_000, MaxOutputTokens: 128_000},
	"claude-haiku-4-5-20251001": {ContextWindowTokens: 200_000, MaxOutputTokens: 64_000},
}

// ResolveModelConfig applies the limits published for an exact Anthropic model identifier.
func ResolveModelConfig(config reasoning.ModelConfig) (reasoning.ModelConfig, error) {
	capabilities, known := modelCapabilities[config.Model]
	if !known {
		return reasoning.ResolveModelCapabilities(config, nil)
	}
	return reasoning.ResolveModelCapabilities(config, &capabilities)
}

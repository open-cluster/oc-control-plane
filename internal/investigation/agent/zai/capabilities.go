package zai

import reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"

var modelCapabilities = map[string]reasoning.ModelCapabilities{
	"glm-4.7": {ContextWindowTokens: 204_800, MaxOutputTokens: 131_072},
}

// ResolveModelConfig applies the limits published for an exact Z.AI model identifier.
func ResolveModelConfig(config reasoning.ModelConfig) (reasoning.ModelConfig, error) {
	capabilities, known := modelCapabilities[config.Model]
	if !known {
		return reasoning.ResolveModelCapabilities(config, nil)
	}
	return reasoning.ResolveModelCapabilities(config, &capabilities)
}

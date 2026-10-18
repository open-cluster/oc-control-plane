package agent_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

func TestModelBudgetsAreApplicationAllowances(t *testing.T) {
	for _, provider := range []string{"anthropic", "openrouter"} {
		config := agent.ModelConfig{Provider: provider, Model: "an-exact-model", Credential: agent.Secret("test-key")}.WithDefaults()
		if config.ContextWindowTokens != 200_000 || config.MaxOutputTokens != 32_000 {
			t.Fatalf("%s application allowances = context %d output %d", provider, config.ContextWindowTokens, config.MaxOutputTokens)
		}
		config.ContextWindowTokens, config.MaxOutputTokens = 64_000, 8_000
		config = config.WithDefaults()
		if config.ContextWindowTokens != 64_000 || config.MaxOutputTokens != 8_000 {
			t.Fatalf("explicit allowances were replaced: %v", config)
		}
	}
}

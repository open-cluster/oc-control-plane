package zai_test

import (
	"strings"
	"testing"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/zai"
)

func TestResolveModelConfigUsesExactModelCapabilities(t *testing.T) {
	resolved, err := zai.ResolveModelConfig(reasoning.ModelConfig{Model: "glm-4.7"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 204_800 || resolved.MaxOutputTokens != 131_072 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
	if _, err = zai.ResolveModelConfig(reasoning.ModelConfig{
		Model: "glm-4.7", MaxOutputTokens: 131_072,
	}); err != nil {
		t.Fatalf("published output boundary: %v", err)
	}
}

func TestResolveModelConfigRequiresBothLimitsForACustomModel(t *testing.T) {
	_, err := zai.ResolveModelConfig(reasoning.ModelConfig{Model: "glm-private", MaxOutputTokens: 32_000})
	if err == nil || !strings.Contains(err.Error(), "context and output") {
		t.Fatalf("error = %v", err)
	}
}

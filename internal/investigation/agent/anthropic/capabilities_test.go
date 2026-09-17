package anthropic_test

import (
	"strings"
	"testing"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/anthropic"
)

func TestResolveModelConfigUsesExactModelCapabilities(t *testing.T) {
	resolved, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 1_000_000 || resolved.MaxOutputTokens != 128_000 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
}

func TestResolveModelConfigRequiresBothLimitsForACustomModel(t *testing.T) {
	_, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{
		Model: "claude-private", ContextWindowTokens: 300_000,
	})
	if err == nil || !strings.Contains(err.Error(), "context and output") {
		t.Fatalf("error = %v", err)
	}

	resolved, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{
		Model: "claude-private", ContextWindowTokens: 300_000, MaxOutputTokens: 48_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 300_000 || resolved.MaxOutputTokens != 48_000 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
}

func TestResolveModelConfigRefusesLimitAboveKnownModel(t *testing.T) {
	_, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{
		Model: "claude-sonnet-5", ContextWindowTokens: 1_000_001,
	})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveModelConfigAcceptsConservativeLimits(t *testing.T) {
	resolved, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{
		Model: "claude-sonnet-5", ContextWindowTokens: 900_000, MaxOutputTokens: 64_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 900_000 || resolved.MaxOutputTokens != 64_000 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
}

func TestResolveModelConfigRequiresContextAboveOutput(t *testing.T) {
	_, err := anthropic.ResolveModelConfig(reasoning.ModelConfig{
		Model: "claude-private", ContextWindowTokens: 64_000, MaxOutputTokens: 64_000,
	})
	if err == nil || !strings.Contains(err.Error(), "must exceed") {
		t.Fatalf("error = %v", err)
	}
}

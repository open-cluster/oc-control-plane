package anthropic_test

import (
	"strings"
	"testing"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/anthropic"
)

func TestResolveDeploymentUsesExactModelCapabilities(t *testing.T) {
	resolved, err := anthropic.ResolveDeployment(reasoning.Deployment{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 1_000_000 || resolved.MaxOutputTokens != 128_000 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
}

func TestResolveDeploymentRequiresBothLimitsForACustomModel(t *testing.T) {
	_, err := anthropic.ResolveDeployment(reasoning.Deployment{
		Model: "claude-private", ContextWindowTokens: 300_000,
	})
	if err == nil || !strings.Contains(err.Error(), "context and output") {
		t.Fatalf("error = %v", err)
	}

	resolved, err := anthropic.ResolveDeployment(reasoning.Deployment{
		Model: "claude-private", ContextWindowTokens: 300_000, MaxOutputTokens: 48_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindowTokens != 300_000 || resolved.MaxOutputTokens != 48_000 {
		t.Fatalf("limits = context %d output %d", resolved.ContextWindowTokens, resolved.MaxOutputTokens)
	}
}

func TestResolveDeploymentRefusesLimitAboveKnownModel(t *testing.T) {
	_, err := anthropic.ResolveDeployment(reasoning.Deployment{
		Model: "claude-sonnet-5", ContextWindowTokens: 1_000_001,
	})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("error = %v", err)
	}
}

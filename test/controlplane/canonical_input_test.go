package controlplane

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

func TestAssignedMessageBeyondPreviewReachesModel(t *testing.T) {
	address := freeAddress(t)
	prompts := make(chan modelagent.Prompt, 4)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{prompts: prompts}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	request := strings.Repeat("context ", 300) + "Use the corrected region eu-west-2 and exclude eu-central-1."
	_, turn := plane.openConversation(t, "complete request", request)
	plane.awaitInvestigation(t, turn)
	select {
	case prompt := <-prompts:
		var rendered strings.Builder
		for _, block := range prompt.Content {
			rendered.WriteString(block.Text)
		}
		if !strings.Contains(rendered.String(), request) {
			t.Fatal("the complete assigned Message did not reach the model")
		}
	default:
		t.Fatal("Investigation did not invoke the model")
	}
}

func TestOversizedAssignedInputRequestsNarrowingWithoutCallingModel(t *testing.T) {
	address := freeAddress(t)
	prompts := make(chan modelagent.Prompt, 4)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
		cfg.ModelContextWindowTokens = 33000
		cfg.ModelMaxOutputTokens = 32000
	}, app.Options{Model: concludingModel{prompts: prompts}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	_, turn := plane.openConversation(t, "oversized input", strings.Repeat("界", 8192))
	final := plane.awaitInvestigation(t, turn)
	var result struct {
		Status      string `json:"status"`
		Limitations []struct {
			Type             string  `json:"type"`
			MessageSequences []int64 `json:"messageSequences"`
		} `json:"limitations"`
	}
	if err := json.Unmarshal([]byte(final), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "needs_input" || len(result.Limitations) != 1 ||
		result.Limitations[0].Type != "essential_human_input" || len(result.Limitations[0].MessageSequences) != 1 ||
		result.Limitations[0].MessageSequences[0] != 1 {
		t.Fatalf("oversized input lacks an explicit persisted sequence refusal: %s", final)
	}
	select {
	case <-prompts:
		t.Fatal("model was called with input that cannot fit")
	default:
	}
}

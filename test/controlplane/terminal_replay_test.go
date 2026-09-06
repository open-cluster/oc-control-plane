package controlplane

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestCompletedInvestigationReplaysOneCanonicalEnding(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	_, turn := plane.openConversation(t, "terminal replay", "what happened?")
	final := plane.awaitInvestigation(t, turn)
	var result struct {
		Summary string `json:"summary"`
	}
	decodeInto(t, final, &result)
	status, replay := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, replay)
	}
	endings := 0
	for _, line := range strings.Split(replay, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Summary string `json:"summary"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "concluded" {
			endings++
			if event.Payload.Summary != result.Summary {
				t.Fatalf("replay summary=%q, result=%q", event.Payload.Summary, result.Summary)
			}
		}
	}
	if endings != 1 {
		t.Fatalf("terminal events=%d, want one: %s", endings, replay)
	}
}

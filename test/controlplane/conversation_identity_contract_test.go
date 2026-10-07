package controlplane

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestWebConversationExposesTrustedSourceAndUserAttribution(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Completer: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}

	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "trusted attribution", "message": "what changed?"})
	if status != http.StatusCreated {
		t.Fatalf("opening Conversation = %d: %s", status, body)
	}
	var opened map[string]json.RawMessage
	decodeInto(t, body, &opened)
	if string(opened["source"]) != `"web"` {
		t.Errorf("source = %s, want web", opened["source"])
	}
	if _, present := opened["surface"]; present {
		t.Errorf("retired surface was exposed: %s", body)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(opened["message"], &message); err != nil {
		t.Fatal(err)
	}
	if string(message["role"]) != `"user"` {
		t.Errorf("role = %s, want user", message["role"])
	}
	if _, present := message["actorKind"]; present {
		t.Errorf("retired actorKind was exposed: %s", body)
	}
	if string(message["actorId"]) == `""` || string(message["actorDisplay"]) == `""` {
		t.Errorf("trusted attribution was not retained: %s", body)
	}
}

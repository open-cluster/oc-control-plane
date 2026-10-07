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
	var id string
	if err := json.Unmarshal(opened["id"], &id); err != nil {
		t.Fatal(err)
	}
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

	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations", nil)
	if status != http.StatusOK {
		t.Fatalf("listing Conversations = %d: %s", status, body)
	}
	var listed struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	decodeInto(t, body, &listed)
	if len(listed.Items) != 1 || string(listed.Items[0]["source"]) != `"web"` {
		t.Fatalf("listed Conversation identity = %s", body)
	}

	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("reading Conversation = %d: %s", status, body)
	}
	var detail struct {
		Source   string                       `json:"source"`
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	decodeInto(t, body, &detail)
	if detail.Source != "web" || len(detail.Messages) != 1 || string(detail.Messages[0]["role"]) != `"user"` {
		t.Fatalf("Conversation detail identity = %s", body)
	}
}

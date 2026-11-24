package controlplane

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestConversationAcceptsExplicitQuestionWindow(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	id, _ := plane.openConversation(t, "historical question", "")
	path := "/conversations/" + id + "/messages"
	for _, invalid := range []map[string]any{
		{"windowFrom": "2026-08-01T08:00:00Z"},
		{"windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": nil, "windowUntil": nil},
		{"windowFrom": "2026-08-01T08:00:00", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T10:00:00Z", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T11:00:00Z", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2099-08-01T08:00:00Z", "windowUntil": "2099-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T08:00:00Z", "windowUntil": "2026-08-01T10:00:00Z", "text": ""},
	} {
		invalid["clientMessageId"] = uuid.NewString()
		if _, exists := invalid["text"]; !exists {
			invalid["text"] = "question"
		}
		status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+path, invalid)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid window accepted: %d %s", status, body)
		}
	}
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
	var conversations struct{ Items []struct{} }
	decodeInto(t, body, &conversations)
	if status != http.StatusOK || len(conversations.Items) != 0 {
		t.Fatalf("invalid windows created Messages: %d %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+path, map[string]any{
		"clientMessageId": uuid.NewString(), "text": "what changed in this interval?",
		"windowFrom": "2026-08-01T10:00:00.123456+02:00", "windowUntil": "2026-08-01T12:00:00.654321+02:00",
	})
	if status != http.StatusAccepted {
		t.Fatalf("create with window: %d %s", status, body)
	}
	var accepted struct {
		Message struct{ WindowFrom, WindowUntil string }
	}
	decodeInto(t, body, &accepted)
	if accepted.Message.WindowFrom != "2026-08-01T08:00:00.123456Z" || accepted.Message.WindowUntil != "2026-08-01T10:00:00.654321Z" {
		t.Fatalf("effective window was not echoed in UTC: %s", body)
	}
	awaitChatMessages(t, plane, path, 2)
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
	var found struct {
		Items []struct{ WindowFrom, WindowUntil string }
	}
	decodeInto(t, body, &found)
	if status != http.StatusOK || len(found.Items) != 2 || found.Items[1].WindowFrom != accepted.Message.WindowFrom ||
		found.Items[1].WindowUntil != accepted.Message.WindowUntil {
		t.Fatalf("Chat reply changed the accepted window: %d %s", status, body)
	}
}

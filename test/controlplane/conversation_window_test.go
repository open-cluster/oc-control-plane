package controlplane

import (
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestConversationAcceptsExplicitQuestionWindow(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	for _, invalid := range []map[string]any{
		{"windowFrom": "2026-08-01T08:00:00Z"},
		{"windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": nil, "windowUntil": nil},
		{"windowFrom": "2026-08-01T08:00:00", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T10:00:00Z", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T11:00:00Z", "windowUntil": "2026-08-01T10:00:00Z"},
		{"windowFrom": "2099-08-01T08:00:00Z", "windowUntil": "2099-08-01T10:00:00Z"},
		{"windowFrom": "2026-08-01T08:00:00Z", "windowUntil": "2026-08-01T10:00:00Z", "message": ""},
	} {
		invalid["subject"] = "invalid window"
		if _, exists := invalid["message"]; !exists {
			invalid["message"] = "question"
		}
		status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations", invalid)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid window accepted: %d %s", status, body)
		}
	}
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations", nil)
	var conversations struct{ Total int }
	decodeInto(t, body, &conversations)
	if status != http.StatusOK || conversations.Total != 0 {
		t.Fatalf("invalid windows created Conversations: %d %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations", map[string]any{
		"subject": "historical question", "message": "what changed in this interval?",
		"windowFrom": "2026-08-01T10:00:00.123456+02:00", "windowUntil": "2026-08-01T12:00:00.654321+02:00",
	})
	if status != http.StatusCreated {
		t.Fatalf("create with window: %d %s", status, body)
	}
	var accepted struct {
		Message struct{ WindowFrom, WindowUntil string }
		Turn    struct{ InvestigationID string }
	}
	decodeInto(t, body, &accepted)
	if accepted.Message.WindowFrom != "2026-08-01T08:00:00.123456Z" || accepted.Message.WindowUntil != "2026-08-01T10:00:00.654321Z" {
		t.Fatalf("effective window was not echoed in UTC: %s", body)
	}
	plane.awaitInvestigation(t, accepted.Turn.InvestigationID)
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+accepted.Turn.InvestigationID, nil)
	var found struct{ WindowFrom, WindowUntil string }
	decodeInto(t, body, &found)
	if status != http.StatusOK || found.WindowFrom != accepted.Message.WindowFrom || found.WindowUntil != accepted.Message.WindowUntil {
		t.Fatalf("Investigation changed the accepted window: %d %s", status, body)
	}
}

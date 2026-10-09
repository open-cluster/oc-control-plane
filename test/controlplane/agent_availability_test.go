package controlplane

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
)

func TestInjectedAgentAllowsConversationWithoutConfiguredProvider(t *testing.T) {
	plane := startIntegrationPlaneWithOptions(t, app.Options{Agent: &blockingAgentMain{}})
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "injected Agent", "message": "investigate checkout"})
	if status != http.StatusCreated {
		t.Fatalf("injected Agent was treated as unavailable = %d: %s", status, body)
	}
}

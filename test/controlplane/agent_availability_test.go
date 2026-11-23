package controlplane

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/app"
)

func TestInjectedAgentAllowsConversationWithoutConfiguredProvider(t *testing.T) {
	plane := startIntegrationPlaneWithOptions(t, app.Options{Model: &blockingModel{}})
	id, _ := plane.openConversation(t, "injected Agent", "")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations/"+id+"/messages",
		map[string]any{"clientMessageId": uuid.NewString(), "text": "What changed in checkout?"})
	if status != http.StatusAccepted {
		t.Fatalf("injected Agent was treated as unavailable = %d: %s", status, body)
	}
}

package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapabilityMetadataReportsDeploymentFeaturesWithoutPrivateConfiguration(t *testing.T) {
	handler := Handlers{ConversationsEnabled: false}
	response := httptest.NewRecorder()
	handler.meta(response, httptest.NewRequest("GET", "/api/v1/meta", nil))

	if response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
	var body capabilityMetadataView
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Capabilities) == 0 {
		t.Fatal("capability metadata is empty")
	}
	found := false
	for _, capability := range body.Capabilities {
		if capability.Key == "conversations" {
			found = true
			if capability.Enabled || capability.Availability != "unavailable" {
				t.Fatalf("conversations = %#v", capability)
			}
		}
	}
	if !found {
		t.Fatal("conversations capability is absent")
	}
	for _, forbidden := range []string{"provider", "model", "secret", "database"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("metadata exposes private composition term %q: %s", forbidden, response.Body.String())
		}
	}
}

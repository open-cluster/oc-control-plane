package kubernetes

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Verification judges the bound Relay's own state, and every answer names what is wrong in
// the operator's language: a missing binding, a dead session, or a capability the Relay
// did not advertise.
func TestVerify_JudgesTheRelayHonestly(t *testing.T) {
	t.Parallel()

	all := Definition().Capabilities

	for _, testCase := range []struct {
		name       string
		relay      integrations.RelayStatus
		wantStatus integrations.Status
		wantInNote string
	}{
		{"no relay bound",
			integrations.RelayStatus{},
			integrations.StatusFailed, "no relay serves"},
		{"relay not connected",
			integrations.RelayStatus{Bound: true},
			integrations.StatusFailed, "not connected"},
		{"relay missing a capability",
			integrations.RelayStatus{Bound: true, Connected: true, Capabilities: all[:1]},
			integrations.StatusDegraded, "does not advertise"},
		{"relay advertising everything",
			integrations.RelayStatus{Bound: true, Connected: true, Capabilities: all},
			integrations.StatusActive, "every capability"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			verified := Definition().Verify(integrations.VerifyInput{Relay: testCase.relay})
			if verified.Status != testCase.wantStatus {
				t.Errorf("verified as %v, want %v; note: %s",
					verified.Status, testCase.wantStatus, verified.Note)
			}
			if !strings.Contains(verified.Note, testCase.wantInNote) {
				t.Errorf("the note %q does not say %q", verified.Note, testCase.wantInNote)
			}
		})
	}
}

func TestDefinition_DeclaresTheRelayShape(t *testing.T) {
	t.Parallel()

	definition := Definition()
	if definition.ID != integrations.TypeKubernetes || definition.Key != "kubernetes" {
		t.Errorf("the definition's identity is (%d, %q)", definition.ID, definition.Key)
	}
	if definition.Category != integrations.Category("infrastructure") {
		t.Errorf("category = %q, want infrastructure", definition.Category)
	}
	if !definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("kubernetes is relay-served and receives no webhooks")
	}
	if len(definition.Capabilities) != 3 {
		t.Errorf("kubernetes declares %d capabilities, want the three typed reads",
			len(definition.Capabilities))
	}
	const wantDescription = "Give investigations a read-only inventory of Kubernetes " +
		"workloads through an outbound Relay."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

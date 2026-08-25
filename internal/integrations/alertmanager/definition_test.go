package alertmanager

import (
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Verification is honest about what it can establish. OpenCluster never calls an
// Alertmanager, so the only proof the path works is a delivery that actually arrived —
// and its absence is stated as an absence, never dressed up as a check.
func TestVerify_SaysWhatADeliveryProved(t *testing.T) {
	t.Parallel()

	fresh := Definition().Verify(integrations.VerifyInput{})
	if fresh.Status != integrations.StatusConfigured {
		t.Errorf("an integration nothing has delivered to verifies as %v, want configured",
			fresh.Status)
	}
	if !strings.Contains(fresh.Note, "nothing has arrived yet") {
		t.Errorf("the note %q does not tell the operator what to do next", fresh.Note)
	}

	delivered := Definition().Verify(integrations.VerifyInput{
		LastAcceptedDelivery: time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
	})
	if delivered.Status != integrations.StatusActive {
		t.Errorf("an integration that accepted a delivery verifies as %v, want active",
			delivered.Status)
	}
	if !strings.Contains(delivered.Note, "2026-01-02T15:04:05Z") {
		t.Errorf("the note %q does not say when the proof arrived", delivered.Note)
	}
}

// The definition's metadata mirrors the seeded integration_type row; the cross-check
// against the database lives at the composition-root seam, and this pins the compiled half.
func TestDefinition_DeclaresTheInboundShape(t *testing.T) {
	t.Parallel()

	definition := Definition()
	if definition.ID != integrations.TypeAlertmanager || definition.Key != "alertmanager" {
		t.Errorf("the definition's identity is (%d, %q)", definition.ID, definition.Key)
	}
	if !definition.ReceivesWebhooks || definition.RequiresRelay {
		t.Error("alertmanager is reached inbound and needs no relay")
	}
	const wantDescription = "Create incidents from firing and resolved Alertmanager alerts " +
		"delivered through an authenticated webhook."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

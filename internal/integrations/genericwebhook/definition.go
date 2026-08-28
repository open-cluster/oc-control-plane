package genericwebhook

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Definition declares the canonical inbound adapter for alert sources without a
// first-class provider integration.
func Definition() integrations.Definition {
	return integrations.Definition{
		ID:               integrations.TypeGenericWebhook,
		Key:              "generic_webhook",
		Name:             "Generic Webhook",
		Description:      "Create incidents from canonical firing and resolved Alert Events delivered through an authenticated webhook.",
		Category:         integrations.CategoryAlerting,
		ReceivesWebhooks: true,
		Verify:           verify,
	}
}

func verify(input integrations.VerifyInput) integrations.Verification {
	if input.LastAcceptedDelivery.IsZero() {
		return integrations.Verification{
			Status: integrations.StatusConfigured,
			Note:   "configured to accept deliveries; no canonical Alert Event has arrived yet",
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note: "a canonical Alert Event was accepted at " +
			input.LastAcceptedDelivery.UTC().Format(time.RFC3339),
	}
}

package genericwebhook

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Definition declares the canonical inbound adapter for alert sources without a
// first-class provider integration.
func Definition() integrations.Definition {
	return integrations.Definition{
		Manifest: integrations.Manifest{
			Type: integrations.TypeGenericWebhook, Key: "generic_webhook", Name: "Generic Webhook",
			Description:       "Create incidents from canonical firing and resolved Alert Events delivered through an authenticated webhook.",
			Category:          integrations.CategoryAlerting,
			DocumentationSlug: "integrations/alerting/generic_webhook",
		},
		Verify: verify,
	}
}

func verify(input integrations.VerifyInput) integrations.Verification {
	if input.LastAcceptedDelivery.IsZero() {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "configured to accept deliveries; no canonical Alert Event has arrived yet",
		}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified,
		Note: "a canonical Alert Event was accepted at " +
			input.LastAcceptedDelivery.UTC().Format(time.RFC3339),
	}
}

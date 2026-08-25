package alertmanager

import (
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
func Definition() integrations.Definition {
	return integrations.Definition{
		ID:   integrations.TypeAlertmanager,
		Key:  "alertmanager",
		Name: "Prometheus Alertmanager",
		Description: "Create incidents from firing and resolved Alertmanager alerts " +
			"delivered through an authenticated webhook.",
		Logo:     "alertmanager",
		Category: integrations.CategoryAlerting,
		DocumentationURL: "https://prometheus.io/docs/alerting/latest/configuration/" +
			"#webhook_config",
		// NOTHING TO CONFIGURE, and saying so is the honest definition. Alertmanager is
		// reached inbound: the platform mints the secret and the Integration carries its
		// own operator-chosen name. A "source name" field would be a second place to write
		// what the Integration is called, and the two would eventually disagree.
		Config:           nil,
		RequiresRelay:    false,
		ReceivesWebhooks: true,
		Verify:           verify,
	}
}

// verify judges this integration from what has actually happened. There is nothing to
// probe outbound — OpenCluster never calls an Alertmanager — so the honest verification is
// the delivery record: a delivery that arrived and was accepted is proof the customer's
// Alertmanager can reach the endpoint, and its absence is stated as an absence rather than
// dressed up as a check.
func verify(input integrations.VerifyInput) integrations.Verification {
	if input.LastAcceptedDelivery.IsZero() {
		return integrations.Verification{
			Status: integrations.StatusConfigured,
			Note: "configured to accept deliveries; nothing has arrived yet — add the " +
				"webhook URL and secret to your Alertmanager and send a test alert to prove " +
				"the path",
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note: "a delivery was accepted at " +
			input.LastAcceptedDelivery.UTC().Format("2006-01-02T15:04:05Z") +
			"; your Alertmanager can reach this endpoint",
	}
}

package alertmanager

import (
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func Definition() integrations.Definition {
	return integrations.Definition{
		Manifest: integrations.Manifest{
			Key:  "alertmanager",
			Name: "Prometheus Alertmanager",
			Description: "Create incidents from firing and resolved Alertmanager alerts " +
				"delivered through an authenticated webhook.",
			Logo: "alertmanager", Category: integrations.CategoryAlerting,
			SourceURL:         "https://prometheus.io/docs/alerting/latest/configuration/#webhook_config",
			DocumentationSlug: "integrations/alerting/alertmanager",
			Config:            nil,
		},
		Verify: verify,
	}
}

func verify(input integrations.VerifyInput) integrations.Verification {
	if input.LastAcceptedDelivery.IsZero() {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note: "configured to accept deliveries; nothing has arrived yet — add the " +
				"webhook URL and secret to your Alertmanager and send a test alert to prove " +
				"the path",
		}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified,
		Note: "a delivery was accepted at " +
			input.LastAcceptedDelivery.UTC().Format("2006-01-02T15:04:05Z") +
			"; your Alertmanager can reach this endpoint",
	}
}

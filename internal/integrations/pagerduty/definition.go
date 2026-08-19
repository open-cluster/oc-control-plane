package pagerduty

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting PagerDuty makes available. All reads; nothing here
// acknowledges, resolves, or pages anyone.
const (
	ListIncidents = "pagerduty.list_incidents"
	GetIncident   = "pagerduty.get_incident"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The client is passed in because its base URL is deployment configuration: production
// reaches the vendor, a test reaches a fake, and the code in between is the same either
// way — that is the provider transport seam.
func Definition(client *Client) integrations.Definition {
	return integrations.Definition{
		ID:               integrations.TypePagerDuty,
		Key:              "pagerduty",
		Name:             "PagerDuty",
		Description:      "Read incidents from your account: bounded, read-only on-call context.",
		Logo:             "pagerduty",
		Category:         integrations.CategoryIncident,
		DocumentationURL: "https://developer.pagerduty.com/api-reference/",
		Capabilities:     []string{ListIncidents, GetIncident},
		Config: []integrations.Field{
			{
				Name:  "apiToken",
				Title: "API access key",
				Description: "A REST API key with read access, from your account's API " +
					"Access Keys page. It is verified live against PagerDuty before being " +
					"saved, stored sealed, and never shown again.",
				Type:     integrations.FieldString,
				Required: true,
				Secret:   true,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			return probe(ctx, client, input.Credential)
		},
		Tools: tools(client),
	}
}

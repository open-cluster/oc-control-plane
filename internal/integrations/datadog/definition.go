package datadog

import (
	"context"
	"errors"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting Datadog makes available. All reads; nothing here mutes,
// resolves, or edits a monitor.
const (
	ListMonitors = "datadog.list_monitors"
	GetMonitor   = "datadog.get_monitor"
)

// sites are the vendor's own regional origins. A read against the wrong one answers
// exactly like a bad credential — Datadog does not redirect between them — so the site is
// closed to this set rather than typed as free text.
var sites = []string{
	"datadoghq.com", "us3.datadoghq.com", "us5.datadoghq.com",
	"datadoghq.eu", "ap1.datadoghq.com", "ddog-gov.com",
}

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The client is passed in because its base URL is deployment configuration: production
// reaches the vendor, a test reaches a fake, and the code in between is the same either
// way — that is the provider transport seam.
func Definition(client *Client) integrations.Definition {
	return integrations.Definition{
		ID:               integrations.TypeDatadog,
		Key:              "datadog",
		Name:             "Datadog",
		Description:      "Read monitors from your account: bounded, read-only alert state.",
		Logo:             "datadog",
		Category:         integrations.CategoryObservability,
		DocumentationURL: "https://docs.datadoghq.com/api/latest/authentication/",
		Capabilities:     []string{ListMonitors, GetMonitor},
		Config: []integrations.Field{
			{
				Name:  "site",
				Title: "Datadog site",
				Description: "The regional origin your account lives on, from your " +
					"account's Organization Settings page.",
				Type:     integrations.FieldString,
				Required: true,
				Enum:     sites,
				Default:  "datadoghq.com",
			},
			{
				Name:  "credential",
				Title: "API key and application key",
				Description: "A read needs both: paste `{\"apiKey\":\"…\",\"appKey\":\"…\"}` " +
					"using this account's API key and an application key scoped to it. It is " +
					"verified live against Datadog before being saved, stored sealed, and " +
					"never shown again.",
				Type:     integrations.FieldString,
				Required: true,
				Secret:   true,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			site, err := siteOf(input.Integration)
			if err != nil {
				return integrations.Verification{
					Status: integrations.StatusFailed,
					Note:   "the integration carries no usable site; set site and verify again",
				}
			}
			return probe(ctx, client, site, input.Credential)
		},
		Tools: tools(client),
	}
}

func siteOf(integration integrations.Integration) (string, error) {
	site, isText := integration.Configuration["site"].(string)
	if !isText || site == "" {
		return "", errors.New("site is not set on this integration")
	}
	return site, nil
}

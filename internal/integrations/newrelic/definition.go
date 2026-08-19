package newrelic

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting New Relic makes available. All reads; nothing here
// acknowledges, closes, or edits an issue.
const (
	ListIssues = "newrelic.list_issues"
	GetIssue   = "newrelic.get_issue"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
func Definition(client *Client) integrations.Definition {
	return integrations.Definition{
		ID:          integrations.TypeNewRelic,
		Key:         "newrelic",
		Name:        "New Relic",
		Description: "Read correlated issues from your account: bounded, read-only alert context.",
		Logo:        "newrelic",
		Category:    integrations.CategoryObservability,
		DocumentationURL: "https://docs.newrelic.com/docs/apis/nerdgraph/get-started/" +
			"introduction-new-relic-nerdgraph/",
		Capabilities: []string{ListIssues, GetIssue},
		Config: []integrations.Field{
			{
				Name:  "region",
				Title: "Region",
				Description: "The data center your account lives in, from your " +
					"account's API keys page.",
				Type:     integrations.FieldString,
				Required: true,
				Enum:     Regions,
				Default:  "us",
			},
			{
				Name:        "accountId",
				Title:       "Account ID",
				Description: "The numeric New Relic account id every query is scoped to.",
				Type:        integrations.FieldInteger,
				Required:    true,
			},
			{
				Name:  "userKey",
				Title: "User key",
				Description: "A user key with read access to this account, from your " +
					"account's API keys page. It is verified live against New Relic before " +
					"being saved, stored sealed, and never shown again.",
				Type:     integrations.FieldString,
				Required: true,
				Secret:   true,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			region, accountID, err := regionAndAccountOf(input.Integration)
			if err != nil {
				return integrations.Verification{
					Status: integrations.StatusFailed,
					Note:   "the integration carries no usable region or account id; set them and verify again",
				}
			}
			return probe(ctx, client, region, accountID, input.Credential)
		},
		Tools: tools(client),
	}
}

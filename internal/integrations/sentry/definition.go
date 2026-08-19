package sentry

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting Sentry makes available. All reads; nothing here resolves,
// assigns, or comments on an issue.
const (
	ListIssues = "sentry.list_issues"
	GetIssue   = "sentry.get_issue"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The client is passed in because its base URL is deployment configuration: production
// reaches the vendor, a test reaches a fake, and the code in between is the same either
// way — that is the provider transport seam.
func Definition(client *Client) integrations.Definition {
	return integrations.Definition{
		ID:               integrations.TypeSentry,
		Key:              "sentry",
		Name:             "Sentry",
		Description:      "Read issues from your projects: bounded, read-only error and event context.",
		Logo:             "sentry",
		Category:         integrations.CategoryObservability,
		DocumentationURL: "https://docs.sentry.io/api/auth/",
		Capabilities:     []string{ListIssues, GetIssue},
		Config: []integrations.Field{
			{
				Name:  "organizationSlug",
				Title: "Organization slug",
				Description: "The organization's URL slug, from your Sentry organization's " +
					"settings. Every read is scoped to it.",
				Type:     integrations.FieldString,
				Required: true,
			},
			{
				Name:  "authToken",
				Title: "Auth token",
				Description: "An internal integration's auth token, scoped to org:read, " +
					"project:read and event:read. It is verified live against Sentry before " +
					"being saved, stored sealed, and never shown again.",
				Type:     integrations.FieldString,
				Required: true,
				Secret:   true,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			organizationSlug, err := organizationSlugOf(input.Integration)
			if err != nil {
				return integrations.Verification{
					Status: integrations.StatusFailed,
					Note:   "the integration carries no usable organization slug; set organizationSlug and verify again",
				}
			}
			return probe(ctx, client, input.Credential, organizationSlug)
		},
		Tools: tools(client),
	}
}

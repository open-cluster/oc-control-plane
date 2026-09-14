package slack

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func Definition(client *Client, installer *Installer, servesEvents bool) integrations.Definition {
	connection := connect(installer, client)
	return integrations.Definition{
		Manifest: integrations.Manifest{
			Type: integrations.TypeSlack,
			Key:  "slack",
			Name: "Slack",
			Description: "Give investigations read-only access to Slack conversations visible " +
				"to the connected token and reply to direct app mentions in their original thread.",
			Logo:              "slack",
			Category:          integrations.CategoryCollaboration,
			SourceURL:         "https://api.slack.com/authentication/token-types#bot",
			DocumentationSlug: "integrations/collaboration/slack",
			Config: []integrations.Field{
				{
					Key:      "botToken",
					Label:    "Slack token",
					Type:     integrations.FieldString,
					Required: true,
					Secret:   true,
				},
			},
			Tools: tools(client),
		},
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			return probe(ctx, client, input.Credential)
		},
		Inbound: func(integration integrations.Integration) integrations.InboundAvailability {
			if !servesEvents {
				return integrations.InboundAvailability{
					Reason: "this deployment has no Slack signing secret for inbound app mentions",
				}
			}
			if integration.Installation == nil {
				return integrations.InboundAvailability{
					Reason: "app mentions require an installed Slack app; pasted tokens only support reading",
				}
			}
			return integrations.InboundAvailability{Available: true}
		},
		Connect: connection,
	}
}

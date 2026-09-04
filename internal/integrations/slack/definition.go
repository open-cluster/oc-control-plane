package slack

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The bounded Slack Tools offered to Investigations.
const (
	ListChannels       = "slack.list_channels"
	ReadChannelHistory = "slack.read_channel_history"
	ReadThreads        = "slack.read_threads"
	SearchMessages     = "slack.search_messages"
)

func Definition(client *Client, installer *Installer, servesEvents bool) integrations.Definition {
	connection := connect(installer, client)
	return integrations.Definition{
		Manifest: integrations.Manifest{
			ID: integrations.TypeSlack, Key: "slack", Name: "Slack",
			Description: "Give investigations read-only access to Slack conversations visible " +
				"to the connected token and reply to direct app mentions in their original thread.",
			Logo: "slack", Category: integrations.CategoryCollaboration, Available: true,
			SourceURL:         "https://api.slack.com/authentication/token-types#bot",
			DocumentationSlug: "integrations/collaboration/slack",
			Config: []integrations.Field{
				{
					// The field accepts user tokens too, but its name is the key deployed
					// integrations already store their sealed value under, so it stays.
					Name:  "botToken",
					Title: "Slack token",
					Description: "A bot token (xoxb-…) for public-channel reads, or a user " +
						"token (xoxp-… or xoxe.xoxp-…) for message search. It is verified live " +
						"against Slack before being saved, stored sealed, and never shown again.",
					Type:     integrations.FieldString,
					Required: true,
					Secret:   true,
				},
				{
					// Written by the installation flow, never typed. It is declared because
					// every configuration key must be, and because an operator reading the
					// record should be able to see which workspace it names.
					Name:  TeamIDField,
					Title: "Workspace ID",
					Description: "The Slack workspace this integration is installed in, " +
						"recorded by the connect flow. Not a secret, and not something to " +
						"fill in by hand.",
					Type: integrations.FieldString,
					// Its PRESENCE says this integration is an app installation rather than
					// a pasted credential.
					// If it could be typed, an operator could make a pasted token claim an
					// agent that will never answer.
					Recorded: true,
				},
				{
					Name:  AppIDField,
					Title: "Slack app ID",
					Description: "The Slack app the workspace installed, recorded by the " +
						"connect flow.",
					Type:     integrations.FieldString,
					Recorded: true,
				},
			},
			RequiresRelay:    false,
			ReceivesWebhooks: false,
			SupportsConnect:  connection != nil,
			Tools:            tools(client),
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
			team, _ := integration.Configuration[TeamIDField].(string)
			application, _ := integration.Configuration[AppIDField].(string)
			if team == "" || application == "" {
				return integrations.InboundAvailability{
					Reason: "app mentions require an installed Slack app; pasted tokens only support reading",
				}
			}
			return integrations.InboundAvailability{Available: true}
		},
		Connect: connection,
	}
}

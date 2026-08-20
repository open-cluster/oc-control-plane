package slack

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting Slack makes available. All reads; posting anything is out
// of scope by decision, so connecting OpenCluster cannot say a word in anyone's
// workspace.
const (
	ListChannels       = "slack.list_channels"
	ReadChannelHistory = "slack.read_channel_history"
	ReadThreads        = "slack.read_threads"
	SearchMessages     = "slack.search_messages"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The client is passed in because its base URL is deployment configuration: production
// reaches the vendor, a test reaches a fake, and the code in between is the same either
// way — that is the provider transport seam.
func Definition(client *Client) integrations.Definition {
	return integrations.Definition{
		ID:   integrations.TypeSlack,
		Key:  "slack",
		Name: "Slack",
		Description: "Give investigations read-only access to Slack conversations visible " +
			"to the connected token; OpenCluster never posts to Slack.",
		Logo:             "slack",
		Category:         integrations.CategoryCollaboration,
		DocumentationURL: "https://api.slack.com/authentication/token-types#bot",
		Capabilities:     []string{ListChannels, ReadChannelHistory, ReadThreads, SearchMessages},
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
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			return probe(ctx, client, input.Credential)
		},
		Tools: tools(client),
	}
}

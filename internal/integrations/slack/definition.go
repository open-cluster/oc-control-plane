package slack

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func Definition(client *Client, installer *Installer) integrations.Definition {
	return integrations.Definition{
		Key:  "slack",
		Name: "Slack",
		Description: "Give investigations read-only access to Slack conversations visible " +
			"to the connected token and reply to direct app mentions in their original thread.",
		Category: integrations.CategoryCollaboration,
		Config: []integrations.Field{
			{
				Key:      "botToken",
				Type:     integrations.FieldString,
				Required: true,
				Secret:   true,
			},
		},
		Tools: tools(client),
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			return probe(ctx, client, input.Credential)
		},
		Connect: connect(installer, client),
	}
}

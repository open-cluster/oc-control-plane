package slack

import (
	"context"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The capabilities connecting Slack makes available.
//
// The read capabilities came first and are unchanged. The three inbound ones are what
// makes Slack a SURFACE rather than a source: a place a Conversation lives. They depend on
// an app installation and on this deployment serving an events endpoint, neither of which
// a grant can express, which is why this provider judges its own availability.
const (
	ListChannels       = "slack.list_channels"
	ReadChannelHistory = "slack.read_channel_history"
	ReadThreads        = "slack.read_threads"
	SearchMessages     = "slack.search_messages"

	// AgentConversations is OpenCluster answering in a Slack DM as an agent.
	AgentConversations = "slack.agent_conversations"
	// Mentions is @OpenCluster in a channel the app is in.
	Mentions = "slack.mentions"
	// ThreadReplies is answering inside the thread the question was asked in.
	ThreadReplies = "slack.thread_replies"
	// PrivateChannels is reading private channels the app was explicitly invited to.
	PrivateChannels = "slack.private_channels"
)

// surfaceCapabilities are the declared capabilities NO TOOL exercises.
//
// Every other capability is kept by exactly one tool, and a test holds that line: a
// capability with no tool is normally a promise the catalog makes and nothing keeps. These
// four are the deliberate exceptions, and they are listed here so that adding a fifth is a
// decision somebody makes on purpose rather than a guard quietly going slack.
//
// What keeps them instead: the inbound three are kept by the events endpoint and the
// delivery worker — OpenCluster being spoken to is not a read an investigation performs —
// and private channels widen the reach of the history tools rather than adding one.
var surfaceCapabilities = []string{
	AgentConversations, Mentions, ThreadReplies, PrivateChannels,
}

// inboundCapabilities are the ones that need an app installation to route events to and a
// deployment that serves the events endpoint.
var inboundCapabilities = []string{AgentConversations, Mentions, ThreadReplies}

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
//
// The client is passed in because its base URL is deployment configuration: production
// reaches the vendor, a test reaches a fake, and the code in between is the same either
// way — that is the provider transport seam. The installer and the signing secret are the
// same kind of thing: a deployment that registered a Slack app offers one-click install
// and can receive events, and one that did not keeps the pasted-token form and says so.
func Definition(client *Client, installer *Installer, servesEvents bool) integrations.Definition {
	definition := integrations.Definition{
		ID:   integrations.TypeSlack,
		Key:  "slack",
		Name: "Slack",
		Description: "Give investigations read-only access to Slack conversations visible " +
			"to the connected token; OpenCluster never posts to Slack.",
		Logo:             "slack",
		Category:         integrations.CategoryCollaboration,
		DocumentationURL: "https://api.slack.com/authentication/token-types#bot",
		Capabilities: []string{
			ListChannels, ReadChannelHistory, ReadThreads, SearchMessages,
			AgentConversations, Mentions, ThreadReplies, PrivateChannels,
		},
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
			},
			{
				Name:  AppIDField,
				Title: "Slack app ID",
				Description: "The Slack app the workspace installed, recorded by the " +
					"connect flow.",
				Type: integrations.FieldString,
			},
		},
		RequiresRelay:    false,
		ReceivesWebhooks: false,
		Probe: func(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
			return probe(ctx, client, input.Credential)
		},
		Tools:   tools(client),
		Connect: connect(installer, client),
	}

	// Assigned after the value exists so the closure can call the generic join on it. The
	// closure captures the variable, so it sees this assignment — and it calls
	// GrantedCapabilityStates, which ignores the override and therefore cannot recurse.
	definition.CapabilityStates = func(found integrations.Integration) []integrations.CapabilityState {
		return capabilityStates(definition, found, servesEvents)
	}
	return definition
}

// capabilityStates judges Slack's capabilities against verified grants AND against what
// this deployment is configured to do.
//
// The inbound three are the reason this override exists. Whether OpenCluster can be spoken
// to in Slack depends on two things no Integration field carries on its own: this
// deployment serving an events endpoint at all, and this integration naming a workspace
// installation those events can be routed to. A pasted token names none — it is a
// credential for reading, not an app somebody installed — so it reports the inbound
// capabilities as unavailable and says why, rather than claiming an agent that will never
// answer.
func capabilityStates(
	definition integrations.Definition, found integrations.Integration, servesEvents bool,
) []integrations.CapabilityState {
	installed, _ := found.Configuration[TeamIDField].(string)

	granted := make(map[string]bool, len(found.VerifyGrants))
	for _, grant := range found.VerifyGrants {
		granted[grant] = true
	}

	states := definition.GrantedCapabilityStates(found)
	for index, state := range states {
		switch state.Capability {
		case PrivateChannels:
			// No tool of its own — it widens what the history tools may reach — so the
			// generic join has nothing to gate it on and would call it available.
			if !granted["groups:history"] {
				states[index] = unavailable(PrivateChannels,
					"this installation was not granted groups:history, so OpenCluster "+
						"reads only public channels it has been invited to")
			}
			continue
		case SearchMessages:
			// Available or not, the reason matters: absence here is a decision rather
			// than a gap, and an operator told only "unavailable" goes looking for a
			// permission to grant.
			if !state.Available {
				states[index] = unavailable(SearchMessages,
					"OpenCluster does not request workspace-wide search: it reasons over "+
						"conversations it has been invited into, not everything an "+
						"employee can see")
			}
			continue
		}
		if !isInbound(state.Capability) {
			continue
		}
		switch {
		case !servesEvents:
			states[index] = unavailable(state.Capability,
				"this deployment serves no Slack events endpoint, so Slack has nowhere "+
					"to deliver a mention")
		case installed == "":
			states[index] = unavailable(state.Capability,
				"this integration was connected with a pasted token, which names no "+
					"workspace installation for Slack to deliver events to; connect Slack "+
					"to make OpenCluster answerable in your workspace")
		default:
			states[index] = integrations.CapabilityState{
				Capability: state.Capability, Available: true,
			}
		}
	}
	return states
}

func isInbound(capability string) bool {
	for _, inbound := range inboundCapabilities {
		if inbound == capability {
			return true
		}
	}
	return false
}

func unavailable(capability, because string) integrations.CapabilityState {
	return integrations.CapabilityState{Capability: capability, Reason: because}
}

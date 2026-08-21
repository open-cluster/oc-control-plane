package slack

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The definition's promises, pinned. These are what the seeded reference row, the setup
// flow and the investigator rely on; a drift here is a product change, not a refactor.

func TestDefinitionMirrorsTheSeededRow(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if definition.ID != integrations.TypeSlack || definition.Key != "slack" {
		t.Errorf("identity = %d %q", definition.ID, definition.Key)
	}
	if definition.Category != integrations.CategoryCollaboration {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("slack is reached outbound with a token; no relay, no webhooks")
	}
	if definition.Verify != nil {
		t.Error("a credential-bearing type verifies by probing live, not from gathered facts")
	}
	if definition.Probe == nil {
		t.Error("no probe; nothing could ever verify a pasted token against the vendor")
	}
	const wantDescription = "Give investigations read-only access to Slack conversations visible " +
		"to the connected token; OpenCluster never posts to Slack."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

func TestTheOnlyConfigurationFieldIsTheSecretToken(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if len(definition.Config) != 1 {
		t.Fatalf("config declares %d fields, want the bot token alone", len(definition.Config))
	}
	token := definition.Config[0]
	if token.Name != "botToken" || !token.Secret || !token.Required {
		t.Errorf("botToken = %+v; it must be required and secret", token)
	}
	if token.Title != "Slack token" {
		t.Errorf("token title = %q, want Slack token", token.Title)
	}
	const wantTokenDescription = "A bot token (xoxb-…) for public-channel reads, or a user " +
		"token (xoxp-… or xoxe.xoxp-…) for message search. It is verified live against " +
		"Slack before being saved, stored sealed, and never shown again."
	if token.Description != wantTokenDescription {
		t.Errorf("token description = %q, want %q", token.Description, wantTokenDescription)
	}
	if !strings.Contains(string(definition.ConfigurationSchema()), `"writeOnly":true`) {
		t.Error("the rendered schema does not say the token is write-only")
	}
}

// Tools and capabilities agree one-to-one in both directions. A capability without a tool
// is a promise the catalog makes and nothing keeps; a tool without a capability is
// behavior the catalog never disclosed.
func TestToolsAndCapabilitiesAgreeOneToOne(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))

	exercised := map[string]int{}
	for _, tool := range definition.Tools {
		exercised[tool.Capability]++
	}
	for _, capability := range definition.Capabilities {
		if exercised[capability] != 1 {
			t.Errorf("capability %s is exercised by %d tools, want exactly one",
				capability, exercised[capability])
		}
		delete(exercised, capability)
	}
	for capability := range exercised {
		t.Errorf("tool capability %s is not one the definition declares", capability)
	}
}

// Every tool carries its full contract. The model chooses by this metadata, so
// an empty field is not missing documentation — it is a tool that will be misrouted and
// then patched with prompts.
func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient("")).Tools {
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "slack.") {
			t.Errorf("tool name %q does not carry the provider prefix", tool.Name)
		}
		for field, value := range map[string]string{
			"description":  tool.Description,
			"whenToUse":    tool.WhenToUse,
			"whenNotToUse": tool.WhenNotToUse,
			"permissions":  tool.Permissions,
			"output":       tool.Output,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s declares no %s", tool.Name, field)
			}
		}
		if tool.Run == nil {
			t.Errorf("%s declares no Run", tool.Name)
		}
		for _, argument := range tool.Arguments {
			if argument.Name == "" || argument.Description == "" || argument.Type == "" {
				t.Errorf("%s argument %+v is missing its name, description or type",
					tool.Name, argument)
			}
		}
	}
}

// The scopes verification checks are exactly the ones the tools claim to need, so a scope
// can be neither demanded for nothing nor needed silently.
func TestVerifiedScopesMatchWhatTheToolsClaim(t *testing.T) {
	t.Parallel()

	// Both halves: a required scope must be needed by something, and an optional one must
	// be worth holding. A scope in neither map is a scope nothing checks.
	known := map[string]string{}
	for scope, cost := range requiredScopes {
		known[scope] = cost
	}
	for scope, cost := range optionalScopes {
		known[scope] = cost
	}

	claimed := map[string]bool{}
	for _, tool := range Definition(NewClient("")).Tools {
		for scope := range known {
			if strings.Contains(tool.Permissions, scope) {
				claimed[scope] = true
			}
		}
	}
	for scope := range known {
		if !claimed[scope] {
			t.Errorf("verification knows %s and no tool claims to need it", scope)
		}
	}
}

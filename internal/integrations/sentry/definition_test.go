package sentry

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The definition's promises, pinned. These are what the seeded reference row, the setup
// flow and the router rely on; a drift here is a product change, not a refactor.

func TestDefinitionMirrorsTheSeededRow(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if definition.ID != integrations.TypeSentry || definition.Key != "sentry" {
		t.Errorf("identity = %d %q", definition.ID, definition.Key)
	}
	if definition.Category != integrations.CategoryObservability {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("sentry is reached outbound with a token; no relay, no webhooks")
	}
	if definition.Verify != nil {
		t.Error("a credential-bearing type verifies by probing live, not from gathered facts")
	}
	if definition.Probe == nil {
		t.Error("no probe; nothing could ever verify a pasted token against the vendor")
	}
}

func TestConfigDeclaresTheSlugAndTheSecretToken(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if len(definition.Config) != 2 {
		t.Fatalf("config declares %d fields, want the organization slug and the auth token",
			len(definition.Config))
	}
	slug, hasSlug := definition.Field("organizationSlug")
	if !hasSlug || !slug.Required || slug.Secret {
		t.Errorf("organizationSlug = %+v; it must be required and not secret", slug)
	}
	token, hasToken := definition.SecretField()
	if !hasToken || token.Name != "authToken" || !token.Required {
		t.Errorf("secret field = %+v, name %q; want authToken, required", token, token.Name)
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

// Every tool carries the full routing discipline. The router chooses by this metadata, so
// an empty field is not missing documentation — it is a tool that will be misrouted and
// then patched with prompts.
func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient("")).Tools {
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "sentry.") {
			t.Errorf("tool name %q does not carry the provider prefix", tool.Name)
		}
		for field, value := range map[string]string{
			"description":  tool.Description,
			"whenToUse":    tool.WhenToUse,
			"whenNotToUse": tool.WhenNotToUse,
			"permissions":  tool.Permissions,
			"rateLimit":    tool.RateLimit,
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

func TestProbeRefusesAnIntegrationWithNoOrganizationSlug(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	verified := definition.Probe(testContext(t), integrations.ProbeInput{
		Integration: integrations.Integration{Configuration: map[string]any{}},
		Credential:  "token",
	})
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed", verified.Status)
	}
	if !strings.Contains(verified.Note, "organizationSlug") {
		t.Errorf("the note %q does not name the missing field", verified.Note)
	}
}

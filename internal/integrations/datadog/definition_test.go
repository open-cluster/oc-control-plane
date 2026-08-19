package datadog

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestDefinitionMirrorsTheSeededRow(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if definition.ID != integrations.TypeDatadog || definition.Key != "datadog" {
		t.Errorf("identity = %d %q", definition.ID, definition.Key)
	}
	if definition.Category != integrations.CategoryObservability {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("datadog is reached outbound with keys; no relay, no webhooks")
	}
	if definition.Verify != nil {
		t.Error("a credential-bearing type verifies by probing live, not from gathered facts")
	}
	if definition.Probe == nil {
		t.Error("no probe; nothing could ever verify a pasted credential against the vendor")
	}
}

func TestConfigDeclaresTheSiteAndTheSecretPair(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if len(definition.Config) != 2 {
		t.Fatalf("config declares %d fields, want the site and the credential pair", len(definition.Config))
	}
	site, hasSite := definition.Field("site")
	if !hasSite || !site.Required || site.Secret || len(site.Enum) == 0 {
		t.Errorf("site = %+v; it must be required, not secret, and closed to the vendor's regions", site)
	}
	secret, hasSecret := definition.SecretField()
	if !hasSecret || secret.Name != "credential" || !secret.Required {
		t.Errorf("secret field = %+v, name %q; want credential, required", secret, secret.Name)
	}
	if !strings.Contains(string(definition.ConfigurationSchema()), `"writeOnly":true`) {
		t.Error("the rendered schema does not say the credential is write-only")
	}
}

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

func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(NewClient("")).Tools {
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "datadog.") {
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
	}
}

func TestProbeRefusesAnIntegrationWithNoSite(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	verified := definition.Probe(testContext(t), integrations.ProbeInput{
		Integration: integrations.Integration{Configuration: map[string]any{}},
		Credential:  testCredentialJSON(t),
	})
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed", verified.Status)
	}
	if !strings.Contains(verified.Note, "site") {
		t.Errorf("the note %q does not name the missing field", verified.Note)
	}
}

package newrelic

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestDefinitionMirrorsTheSeededRow(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if definition.ID != integrations.TypeNewRelic || definition.Key != "newrelic" {
		t.Errorf("identity = %d %q", definition.ID, definition.Key)
	}
	if definition.Category != integrations.CategoryObservability {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("new relic is reached outbound with a key; no relay, no webhooks")
	}
	if definition.Verify != nil {
		t.Error("a credential-bearing type verifies by probing live, not from gathered facts")
	}
	if definition.Probe == nil {
		t.Error("no probe; nothing could ever verify a pasted key against the vendor")
	}
}

func TestConfigDeclaresRegionAccountAndTheSecretKey(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	if len(definition.Config) != 3 {
		t.Fatalf("config declares %d fields, want region, accountId and userKey", len(definition.Config))
	}
	region, hasRegion := definition.Field("region")
	if !hasRegion || !region.Required || region.Secret || len(region.Enum) != 3 {
		t.Errorf("region = %+v; it must be required, not secret, closed to the three regions", region)
	}
	account, hasAccount := definition.Field("accountId")
	if !hasAccount || !account.Required || account.Secret {
		t.Errorf("accountId = %+v; it must be required and not secret", account)
	}
	key, hasKey := definition.SecretField()
	if !hasKey || key.Name != "userKey" || !key.Required {
		t.Errorf("secret field = %+v, name %q; want userKey, required", key, key.Name)
	}
	if !strings.Contains(string(definition.ConfigurationSchema()), `"writeOnly":true`) {
		t.Error("the rendered schema does not say the key is write-only")
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
		if tool.Name == "" || !strings.HasPrefix(tool.Name, "newrelic.") {
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

func TestProbeRefusesAnIntegrationWithNoRegion(t *testing.T) {
	t.Parallel()

	definition := Definition(NewClient(""))
	verified := definition.Probe(testContext(t), integrations.ProbeInput{
		Integration: integrations.Integration{Configuration: map[string]any{"accountId": float64(123)}},
		Credential:  "key",
	})
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed", verified.Status)
	}
}

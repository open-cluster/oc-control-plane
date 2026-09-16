package genericwebhook

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func TestDefinitionDeclaresGenericWebhook(t *testing.T) {
	t.Parallel()

	definition := Definition()
	if definition.Key != "generic_webhook" {
		t.Errorf("identity = %q, want generic_webhook", definition.Key)
	}
	if definition.Category != integrations.CategoryAlerting || definition.RequiresRelay {
		t.Errorf("generic webhook definition has the wrong delivery shape: %+v", definition)
	}
	if definition.Verify == nil || len(definition.Config) != 0 {
		t.Error("generic webhook must verify from accepted deliveries and need no provider config")
	}
}

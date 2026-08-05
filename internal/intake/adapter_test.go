package intake

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/connection"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The Integration vocabulary has two halves in two packages: internal/connection says which
// Integrations exist and what each can do, and this package says which of them a delivery can
// be parsed for. They must not drift.
//
// The failure this prevents is quiet and lands on a customer. An Integration offering the
// trigger role with no adapter here is one an operator can configure, mint a secret for, and
// point their alerting at — and every delivery through it is answered with a retryable error
// forever, because intake reports a missing adapter as its own fault rather than theirs.
//
// The predicate is CONFIGURABLE rather than merely offered, and the distinction is the whole
// point of the catalog's lifecycle. A `planned` provider names the trigger role — that is what
// the definition will mean when its adapter exists — and no Connection can be created against
// it, so nobody can reach the path this gate protects. Asserting on the offered role alone would
// make the gate demand an adapter for every provider the product has ever named, which is the
// same as demanding the catalog list only what is already built.
func TestEveryConfigurableTriggerIntegrationHasAnAdapter(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, integration := range connection.Integrations() {
		if _, _, configurable := connection.Configurable(integration); !configurable {
			continue
		}
		if !connection.Offers(integration, storage.RoleTrigger) {
			continue
		}
		checked++
		if _, ok := adapterFor(string(integration)); !ok {
			t.Errorf("integration %q can be configured as a trigger connection but no adapter "+
				"here can parse its deliveries; an operator could configure it and every "+
				"delivery would be answered with a retryable error forever", integration)
		}
	}
	if checked == 0 {
		t.Fatal("no configurable trigger integration exists; this gate would pass vacuously")
	}
}

// The other half of that distinction, asserted rather than assumed: a provider that names the
// trigger role and has no adapter must be one nobody can configure. If a lifecycle were flipped
// to `general` without the adapter landing, this is what says so.
func TestAPlannedTriggerIntegrationCannotBeConfigured(t *testing.T) {
	t.Parallel()

	for _, integration := range connection.Integrations() {
		if !connection.Offers(integration, storage.RoleTrigger) {
			continue
		}
		if _, ok := adapterFor(string(integration)); ok {
			continue
		}
		if _, _, configurable := connection.Configurable(integration); configurable {
			t.Errorf("integration %q may be configured as a trigger and nothing here can parse "+
				"its deliveries; a lifecycle was moved without its adapter", integration)
		}
	}
}

// And the other direction: an adapter for something no Connection can name is unreachable
// code that will be maintained as though it were live.
func TestEveryAdapterServesAKnownIntegration(t *testing.T) {
	t.Parallel()

	for integration := range adapters {
		if !connection.Known(connection.Integration(integration)) {
			t.Errorf("an adapter is registered for %q, which is not an Integration this build "+
				"has; nothing can ever reach it", integration)
		}
	}
}

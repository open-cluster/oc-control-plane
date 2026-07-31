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
func TestEveryTriggerIntegrationHasAnAdapter(t *testing.T) {
	t.Parallel()

	for _, integration := range connection.Integrations() {
		if !connection.Offers(integration, storage.RoleTrigger) {
			continue
		}
		if _, ok := adapterFor(string(integration)); !ok {
			t.Errorf("integration %q can be configured as a trigger connection but no adapter "+
				"here can parse its deliveries; an operator could configure it and every "+
				"delivery would be answered with a retryable error forever", integration)
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

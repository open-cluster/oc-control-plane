package app

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestHostedAuditForwardingIsAbsentUntilExplicitlyConfigured(t *testing.T) {
	t.Parallel()

	forwarder, err := configuredAuditForwarder(config.Config{}, Options{})
	if err != nil {
		t.Fatalf("configuring a self-hosted process: %v", err)
	}
	if forwarder != nil {
		t.Fatal("a self-hosted process unexpectedly enabled hosted audit delivery")
	}

	forwarder, err = configuredAuditForwarder(config.Config{
		HostedMode: true, WorkOSAPIKey: "sk_hosted_test",
		WorkOSAuditOrganizations: map[string]string{"org-local": "org_workos"},
	}, Options{})
	if err != nil {
		t.Fatalf("configuring an explicitly hosted process: %v", err)
	}
	if forwarder == nil {
		t.Fatal("hosted mode did not compose the asynchronous WorkOS audit sink")
	}
}

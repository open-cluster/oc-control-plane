package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/audit"
)

type processAuditForwarder struct {
	events chan audit.Recorded
}

func (f processAuditForwarder) Forward(_ context.Context, event audit.Recorded) error {
	f.events <- event
	return nil
}

func TestComposedProcessForwardsTheAuthoritativeAuditEventAsynchronously(t *testing.T) {
	forwarded := make(chan audit.Recorded, 32)
	plane := startIntegrationPlaneWithOptions(t, app.Options{
		AuditForwarder: processAuditForwarder{events: forwarded},
	})
	created := plane.createAlertmanager(t, "forwarded from the composed process")

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-forwarded:
			if event.Target.ID != created.Integration.ID {
				continue
			}
			if event.ID == "" || event.Organization != surfaceOrg ||
				event.Action != audit.ActionIntegrationCreated {
				t.Fatalf("forwarded event = %#v", event)
			}
			return
		case <-deadline.C:
			t.Fatal("the committed Audit Event was not forwarded")
		}
	}
}

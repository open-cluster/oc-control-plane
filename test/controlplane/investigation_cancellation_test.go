package controlplane

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type blockingAgentMain struct {
	started chan struct{}
}

func (b *blockingAgentMain) Run(
	ctx context.Context, _ tenancy.Organization, _ investigation.Investigation,
) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestRunningInvestigationCanBeCancelledThroughTheAuthorizedOperatorSurface(t *testing.T) {
	t.Parallel()

	model := &blockingAgentMain{started: make(chan struct{}, 1)}
	plane, _ := agentPlane(t, model)
	incident := plane.openIncident(t, "CheckoutLatency", "cancel-running")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	select {
	case <-model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the investigation never reached its active model exchange")
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("work was refused merely because the worker was busy: %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusTooManyRequests {
		t.Fatalf("work above the pending backlog was not refused: %d: %s", status, body)
	}
	path := plane.base(surfaceOrg) + "/investigations/" + opened.ID + "/cancel"
	status, body = plane.call(t, http.MethodPost, path, nil)
	if status != http.StatusOK {
		t.Fatalf("cancelling an active investigation = %d: %s", status, body)
	}
	var cancelled struct {
		Status string `json:"status"`
	}
	decodeInto(t, body, &cancelled)
	if cancelled.Status != "cancelled" {
		t.Fatalf("terminal investigation status = %q, want cancelled", cancelled.Status)
	}
	if status, body = plane.call(t, http.MethodPost, path, nil); status != http.StatusConflict {
		t.Fatalf("cancelling a terminal investigation = %d, want 409: %s", status, body)
	}
	foreign := plane.base(neighbourOrg) + "/investigations/" + opened.ID + "/cancel"
	if status, body = plane.call(t, http.MethodPost, foreign, nil); status != http.StatusNotFound {
		t.Fatalf("cross-organization cancellation = %d, want 404: %s", status, body)
	}
}

func TestDirectInvestigationCreationRequiresOnlyAnIncidentIdentity(t *testing.T) {
	t.Parallel()

	plane, _ := agentPlane(t, &blockingAgentMain{started: make(chan struct{}, 1)})
	path := plane.base(surfaceOrg) + "/investigations"

	if status, body := plane.call(t, http.MethodPost, path,
		map[string]any{"question": "why is checkout slow?"}); status != http.StatusBadRequest ||
		!strings.Contains(body, "request body is not what this operation accepts") {
		t.Fatalf("opening from a question = %d, want 400: %s", status, body)
	}
	if status, body := plane.call(t, http.MethodPost, path,
		map[string]any{"incidentId": uuid.NewString(), "question": "why?"}); status != http.StatusBadRequest ||
		!strings.Contains(body, "request body is not what this operation accepts") {
		t.Fatalf("opening with an extra question = %d, want 400: %s", status, body)
	}
	if status, body := plane.call(t, http.MethodPost, path, map[string]any{}); status != http.StatusBadRequest ||
		!strings.Contains(body, "give an incidentId") {
		t.Fatalf("opening without an incidentId = %d, want 400: %s", status, body)
	}
	if status, body := plane.call(t, http.MethodPost, path,
		map[string]any{"incidentId": "not-an-identity"}); status != http.StatusBadRequest ||
		!strings.Contains(body, "incidentId is not an identity") {
		t.Fatalf("opening with an invalid incidentId = %d, want 400: %s", status, body)
	}
}

package controlplane

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type blockingInvestigator struct {
	started chan struct{}
}

func (b *blockingInvestigator) OpenExchange(
	context.Context, investigation.Orientation,
) (investigation.Exchange, error) {
	return b, nil
}

func (b *blockingInvestigator) Next(
	ctx context.Context, _ []investigation.CallResult, _ bool, _ string,
) (investigation.Move, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return investigation.Move{}, ctx.Err()
}

func TestRunningInvestigationCanBeCancelledThroughTheAuthorizedOperatorSurface(t *testing.T) {
	t.Parallel()

	model := &blockingInvestigator{started: make(chan struct{}, 1)}
	plane, _ := autonomousPlaneWith(t, model, nil)
	episode := plane.openEpisode(t, "CheckoutLatency", "cancel-running")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episode})
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

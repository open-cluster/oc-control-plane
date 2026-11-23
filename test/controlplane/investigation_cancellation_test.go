package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/google/uuid"
)

type blockingModel struct {
	bufferedModel
	started chan uuid.UUID
}

func (b *blockingModel) Chat(ctx context.Context, prompt []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if b.started != nil {
		text := prompt[len(prompt)-1].GetTextContent("\n")
		_, raw, _ := strings.Cut(*text, "\n")
		var input struct {
			ID uuid.UUID `json:"investigationId"`
		}
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return nil, err
		}
		select {
		case b.started <- input.ID:
		default:
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunningInvestigationCanBeCancelledThroughTheAuthorizedApplicationAPI(t *testing.T) {
	t.Parallel()

	model := &blockingModel{started: make(chan uuid.UUID, 1)}
	plane, _ := agentPlane(t, model)
	incident := plane.openIncident(t, "CheckoutLatency", "cancel-running")
	var runningID string
	select {
	case id := <-model.started:
		runningID = id.String()
	case <-time.After(10 * time.Second):
		t.Fatal("the investigation never reached its active model exchange")
	}
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusAccepted {
		t.Fatalf("work was refused merely because the worker was busy: %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations",
		map[string]any{"incidentId": incident})
	if status != http.StatusTooManyRequests {
		t.Fatalf("work above the pending backlog was not refused: %d: %s", status, body)
	}
	path := plane.base(surfaceOrg) + "/investigations/" + runningID + "/cancel"
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
	foreign := plane.base(neighbourOrg) + "/investigations/" + runningID + "/cancel"
	defer plane.switchOrganization(t, neighbourOrg)()
	if status, body = plane.call(t, http.MethodPost, foreign, nil); status != http.StatusNotFound {
		t.Fatalf("cross-organization cancellation = %d, want 404: %s", status, body)
	}
}

func TestDirectInvestigationCreationRequiresOnlyAnIncidentIdentity(t *testing.T) {
	t.Parallel()

	plane, _ := agentPlane(t, &blockingModel{started: make(chan uuid.UUID, 1)})
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

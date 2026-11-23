package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type emptyReplyModel struct {
	bufferedModel
	reports atomic.Int32
}

func (m *emptyReplyModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if reportRequested(opts) {
		m.reports.Add(1)
		return reportResponse(investigation.Conclusion{
			Status: investigation.Inconclusive, Summary: "The cause is unknown.",
		})
	}
	return &model.ChatResponse{IsLast: true, StopReason: model.StopReasonStop}, nil
}

func TestInvestigationDoesNotManufactureAConversationReply(t *testing.T) {
	model := &emptyReplyModel{}
	plane, _ := agentPlane(t, model)
	base := plane.base(surfaceOrg)
	conversationID, turn := plane.openConversation(t, "checkout", "what happened?")
	var result struct {
		Status           string
		ConclusionStatus string
	}
	decodeInto(t, plane.awaitInvestigation(t, turn), &result)
	if result.Status != "failed" || model.reports.Load() != 0 {
		t.Fatalf("assigned turn without a native assistant Message ended as %q after %d report requests", result.Status, model.reports.Load())
	}
	status, replay := plane.call(t, http.MethodGet, base+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK || strings.Count(replay, "event: failed\n") != 1 || strings.Contains(replay, "event: concluded\n") {
		t.Fatalf("unexpected terminal replay (%d): %s", status, replay)
	}
	status, body := plane.call(t, http.MethodGet, base+"/conversations/"+conversationID, nil)
	var detail struct {
		Messages []struct{ Role string }
	}
	decodeInto(t, body, &detail)
	if status != http.StatusOK || len(detail.Messages) != 1 || detail.Messages[0].Role != "user" {
		t.Fatalf("failed turn changed visible Messages (%d): %s", status, body)
	}

	incident := plane.openIncident(t, "CheckoutUnavailable", "empty-native-reply")
	status, body = plane.call(t, http.MethodGet, base+"/investigations?incidentId="+incident, nil)
	var listed struct {
		Items []struct{ ID string }
	}
	decodeInto(t, body, &listed)
	if status != http.StatusOK || len(listed.Items) != 1 {
		t.Fatalf("expected one autonomous Investigation (%d): %s", status, body)
	}
	decodeInto(t, plane.awaitInvestigation(t, listed.Items[0].ID), &result)
	if result.Status != "concluded" || result.ConclusionStatus != "inconclusive" || model.reports.Load() != 1 {
		t.Fatalf("autonomous Investigation ended as %q with assessment %q after %d report requests",
			result.Status, result.ConclusionStatus, model.reports.Load())
	}
}

func TestCompletedInvestigationReplaysOneCanonicalEnding(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	_, turn := plane.openConversation(t, "terminal replay", "what happened?")
	final := plane.awaitInvestigation(t, turn)
	var result struct {
		Summary string `json:"summary"`
	}
	decodeInto(t, final, &result)
	status, replay := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, replay)
	}
	endings := 0
	for _, line := range strings.Split(replay, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Summary string `json:"summary"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "concluded" {
			endings++
			if event.Payload.Summary != result.Summary {
				t.Fatalf("replay summary=%q, result=%q", event.Payload.Summary, result.Summary)
			}
		}
	}
	if endings != 1 {
		t.Fatalf("terminal events=%d, want one: %s", endings, replay)
	}
}

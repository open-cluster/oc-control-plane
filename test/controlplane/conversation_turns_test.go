package controlplane

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestConversationTurnsPaginateAndRejectInvalidQueries(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	id, first := plane.openConversation(t, "paginated conversation", "first question")
	plane.awaitInvestigation(t, first)
	path := plane.base(surfaceOrg) + "/conversations/" + id
	status, body := plane.call(t, http.MethodPost, path+"/messages", map[string]any{"message": "second question"})
	if status != http.StatusAccepted {
		t.Fatalf("follow-up = %d: %s", status, body)
	}
	var accepted struct {
		Turn struct{ InvestigationID string } `json:"turn"`
	}
	decodeInto(t, body, &accepted)
	plane.awaitInvestigation(t, accepted.Turn.InvestigationID)
	status, body = plane.call(t, http.MethodGet, path+"/turns?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("first page = %d: %s", status, body)
	}
	var page struct {
		Turns []struct {
			InvestigationID string `json:"investigationId"`
			Turn            int    `json:"turn"`
		} `json:"turns"`
		Next *string `json:"next"`
	}
	decodeInto(t, body, &page)
	if len(page.Turns) != 1 || page.Turns[0].InvestigationID != first || page.Turns[0].Turn != 1 || page.Next == nil {
		t.Fatalf("first page = %s", body)
	}
	cursor := url.QueryEscape(*page.Next)
	status, body = plane.call(t, http.MethodGet, path+"/turns?limit=1&cursor="+cursor, nil)
	if status != http.StatusOK {
		t.Fatalf("next page = %d: %s", status, body)
	}
	decodeInto(t, body, &page)
	if len(page.Turns) != 1 || page.Turns[0].InvestigationID != accepted.Turn.InvestigationID || page.Turns[0].Turn != 2 || page.Next != nil {
		t.Fatalf("last page = %s", body)
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=", "limit=1&limit=2", "cursor=", "cursor=bad", "cursor=a&cursor=b", "search=answer", "sort=turn", "unknown=value", "cursor=%zz", "limit=1;sort=turn"} {
		if status, body := plane.call(t, http.MethodGet, path+"/turns?"+query, nil); status != http.StatusBadRequest {
			t.Errorf("query %s = %d: %s", query, status, body)
		}
	}
	other, _ := plane.openConversation(t, "another conversation", "")
	if status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations/"+other+"/turns?cursor="+cursor, nil); status != http.StatusBadRequest {
		t.Errorf("cursor reused across conversations = %d: %s", status, body)
	}
	for _, suffix := range []string{"", "/turns?limit=200"} {
		status, body := plane.call(t, http.MethodGet, path+suffix, nil)
		if status != http.StatusOK {
			t.Fatalf("reading %s = %d: %s", suffix, status, body)
		}
		var fields map[string]json.RawMessage
		decodeInto(t, body, &fields)
		field := "next"
		if suffix == "" {
			field = "turnsNext"
		}
		if string(fields[field]) != "null" {
			t.Errorf("exhausted %s must be present and null: %s", field, body)
		}
	}
}

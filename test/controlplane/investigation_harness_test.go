package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

type concludingModel struct {
	prompts chan<- modelagent.Prompt
}

func (m concludingModel) Complete(_ context.Context, prompt modelagent.Prompt) (modelagent.Completion, error) {
	if m.prompts != nil {
		select {
		case m.prompts <- prompt:
		default:
		}
	}
	document, _ := json.Marshal(map[string]any{
		"status": "answer_only", "summary": "Investigation completed.",
		"impact": map[string]any{
			"status": "unknown", "current_state": "unknown", "affected_services": []string{},
			"affected_users": []string{}, "summary": "Impact is unknown.", "run_refs": []int{},
		},
		"findings": []any{}, "hypotheses": []any{}, "actions": []any{}, "limitations": []any{},
	})
	return modelagent.Completion{Stop: modelagent.StopToolUse, ToolCalls: []modelagent.CompletionCall{{
		ID: "conclude", Name: modelagent.ConcludeToolName, Arguments: document,
	}}}, nil
}

func agentPlane(t *testing.T, agent investigation.Agent) (*integrationPlane, *vendorFake) {
	t.Helper()
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	operatorAddress := freeAddress(t)
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = operatorAddress
		cfg.InvestigationWorkers = 1
		cfg.MaxPendingInvestigationsPerOrganization = 1
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
	}, app.Options{Agent: agent, SlackAPIURL: vendor.URL})
	return &integrationPlane{controlPlane: plane, operator: operatorAddress, intake: operatorAddress}, vendor
}

func (p *integrationPlane) openIncident(t *testing.T, alertname, fingerprint string) string {
	t.Helper()
	created := p.createAlertmanager(t, "Alertmanager for "+alertname)
	payload := []byte(`{"groupKey":"group-` + fingerprint + `","alerts":[{"status":"firing","fingerprint":"` + fingerprint + `","labels":{"alertname":"` + alertname + `","namespace":"payments"},"annotations":{"summary":"it broke"},"startsAt":"` + time.Now().UTC().Add(-30*time.Minute).Format(time.RFC3339) + `"}]}`)
	if status, body := p.deliver(t, created.Integration.ID, created.WebhookSecret, payload); status != http.StatusAccepted {
		t.Fatalf("seeding delivery = %d: %s", status, body)
	}
	return p.incidentByTitle(t, alertname)
}

func (p *integrationPlane) incidentByTitle(t *testing.T, title string) string {
	t.Helper()
	status, body := p.call(t, http.MethodGet, p.base(surfaceOrg)+"/incidents", nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents = %d: %s", status, body)
	}
	var listed struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	decodeInto(t, body, &listed)
	for _, incident := range listed.Items {
		if incident.Title == title {
			return incident.ID
		}
	}
	t.Fatalf("no incident titled %s: %s", title, body)
	return ""
}

func (p *integrationPlane) awaitInvestigation(t *testing.T, id string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, body := p.call(t, http.MethodGet, p.base(surfaceOrg)+"/investigations/"+id, nil)
		if status != http.StatusOK {
			t.Fatalf("reading investigation = %d: %s", status, body)
		}
		var read struct {
			Status string `json:"status"`
		}
		decodeInto(t, body, &read)
		if read.Status != "queued" && read.Status != "investigating" {
			return body
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("investigation %s did not finish", id)
	return ""
}

func (p *integrationPlane) openConversation(t *testing.T, subject, message string) (string, string) {
	t.Helper()
	body := map[string]any{"subject": subject}
	if message != "" {
		body["message"] = message
	}
	status, answer := p.call(t, http.MethodPost, p.base(surfaceOrg)+"/conversations", body)
	if status != http.StatusCreated {
		t.Fatalf("opening conversation = %d: %s", status, answer)
	}
	var opened struct {
		ID   string `json:"id"`
		Turn *struct {
			InvestigationID string `json:"investigationId"`
		} `json:"turn"`
	}
	decodeInto(t, answer, &opened)
	if opened.Turn == nil {
		return opened.ID, ""
	}
	return opened.ID, opened.Turn.InvestigationID
}

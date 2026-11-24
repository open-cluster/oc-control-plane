package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type bufferedModel struct{}

func (bufferedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (bufferedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

type concludingModel struct {
	bufferedModel
	prompts       chan<- []*message.Msg
	factualAnswer string
}

func (m concludingModel) Chat(_ context.Context, prompt []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if !reportRequested(opts) {
		if m.prompts != nil {
			select {
			case m.prompts <- prompt:
			default:
			}
		}
		return investigationRecap(), nil
	}
	if m.factualAnswer != "" {
		return reportResponse(investigation.Conclusion{Summary: m.factualAnswer})
	}
	return reportResponse(investigation.Conclusion{
		Status:  investigation.Inconclusive,
		Summary: "Investigation completed.",
		Impact:  investigation.Impact{Summary: "Impact is not established."},
	})
}

func reportRequested(opts []model.CallOption) bool {
	var options model.CallOptions
	for _, option := range opts {
		option(&options)
	}
	return len(options.Tools) == 1 && options.Tools[0].Function.Name == "generate_structured_output"
}

func investigationRecap() *model.ChatResponse {
	return &model.ChatResponse{
		IsLast: true, StopReason: model.StopReasonStop,
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "No cause established."}},
	}
}

func reportResponse(conclusion investigation.Conclusion) (*model.ChatResponse, error) {
	document, err := json.Marshal(conclusion)
	if err != nil {
		return nil, err
	}
	return toolResponse("report", "generate_structured_output", string(document)), nil
}

func toolResponse(id, name, input string) *model.ChatResponse {
	return &model.ChatResponse{
		IsLast:     true,
		StopReason: model.StopReasonToolCalls,
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: id, Name: name, Input: input,
		}},
	}
}

func agentPlane(t *testing.T, agent model.ChatModel) (*integrationPlane, *vendorFake) {
	t.Helper()
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	apiAddress := freeAddress(t)
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		dsn = cfg.DatabaseDSN
		cfg.HTTPListenAddress = apiAddress
		cfg.InvestigationWorkers = 1
		cfg.MaxPendingInvestigations = 1
	}, app.Options{Model: agent, SlackAPIURL: vendor.URL})
	return &integrationPlane{controlPlane: plane, api: apiAddress, intake: apiAddress, dsn: dsn}, vendor
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
		if read.Status != "queued" && read.Status != "running" {
			return body
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("investigation %s did not finish", id)
	return ""
}

func (p *integrationPlane) openConversation(t *testing.T, subject, message string) (string, string) {
	t.Helper()
	body := map[string]any{"title": subject}
	status, answer := p.call(t, http.MethodPost, p.base(surfaceOrg)+"/conversations", body)
	if status != http.StatusCreated {
		t.Fatalf("opening conversation = %d: %s", status, answer)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, answer, &opened)
	if message == "" {
		return opened.ID, ""
	}
	return opened.ID, p.seedLegacyTurn(t, opened.ID, message)
}

// seedLegacyTurn reproduces a record admitted before the web Chat cutover.
func (p *integrationPlane) seedLegacyTurn(t *testing.T, id, question string) string {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, p.databaseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	turn := uuid.NewString()
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE conversation SET legacy_history = true
			WHERE org_id = $1 AND conversation_id = $2`, surfaceOrg, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO investigation (org_id, investigation_id, conversation_id, turn, subject, window_from, window_until)
			SELECT c.org_id, $3, c.conversation_id,
			       (SELECT coalesce(max(turn), 0) + 1 FROM investigation WHERE org_id = $1 AND conversation_id = $2),
			       c.title, now() - interval '1 day', now()
			  FROM conversation c WHERE c.org_id = $1 AND c.conversation_id = $2`, surfaceOrg, id, turn); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_message
			(org_id, conversation_id, sequence, role, author_id, author_name, text, investigation_id, window_from, window_until)
			SELECT c.org_id, c.conversation_id,
			       (SELECT coalesce(max(sequence), 0) + 1 FROM conversation_message WHERE org_id = $1 AND conversation_id = $2),
			       1, c.created_by, 'Test Administrator', $4, i.investigation_id, i.window_from, i.window_until
			FROM conversation c JOIN investigation i USING (org_id, conversation_id)
			WHERE c.org_id = $1 AND c.conversation_id = $2 AND i.investigation_id = $3`, surfaceOrg, id, turn, question); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_execution (org_id, execution_id, investigation_id) VALUES ($1, $2, $3)`,
			surfaceOrg, uuid.New(), turn)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

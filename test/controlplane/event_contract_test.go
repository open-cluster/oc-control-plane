package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type eventContractModel struct {
	bufferedModel
	calls       int
	tool        string
	observation chan message.ToolResultBlock
}

func (m *eventContractModel) Chat(_ context.Context, prompt []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	if m.calls == 1 {
		return toolResponse("channels", m.tool, `{"input":{}}`), nil
	}
	if !reportRequested(opts) {
		return investigationRecap(), nil
	}
	conclusion := investigation.Conclusion{
		Status: investigation.Inconclusive, Summary: "line one\n" + strings.Repeat("界", 4087),
	}
	for _, msg := range prompt {
		for _, block := range msg.Content {
			observation, ok := block.(message.ToolResultBlock)
			if !ok || observation.ID != "channels" {
				continue
			}
			select {
			case m.observation <- observation:
			default:
			}
			observed := strings.TrimPrefix(observation.GetOutputText(), "[Tool Run ")
			id, _, _ := strings.Cut(observed, "]")
			if runID, err := uuid.Parse(id); err == nil {
				conclusion.Findings = []investigation.Finding{{
					Kind: investigation.FindingObservation, Statement: "Slack returned the Incident channel.",
					EvidenceRefs: []investigation.EvidenceRef{{ToolRunID: runID}},
				}}
			}
		}
	}
	return reportResponse(conclusion)
}

func TestAvailableToolAndAnswerEventsMatchVersionTwoSchema(t *testing.T) {
	assertModelEventSchema(t, &eventContractModel{tool: "slack_list_channels"},
		[]string{"started", "tool_started", "tool_completed", "concluded"})
}

func assertModelEventSchema(t *testing.T, model *eventContractModel, want []string) {
	t.Helper()
	model.observation = make(chan message.ToolResultBlock, 1)
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.channels = `{"ok":true,"channels":[{"id":"C0INCIDENT","name":"incidents"}]}`
	address := freeAddress(t)
	var dsn string
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		dsn = cfg.DatabaseDSN
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Model: model, SlackAPIURL: vendor.URL})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address, dsn: dsn}
	if status, body := plane.createSlack(t, "Operator testimony", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("create Slack = %d: %s", status, body)
	}
	conversationID, turn := plane.openConversation(t, "event schema", "investigate checkout")
	result := plane.awaitInvestigation(t, turn)
	var final struct {
		Summary string `json:"summary"`
		Runs    []struct {
			ID uuid.UUID
		}
	}
	decodeInto(t, result, &final)
	if len(final.Runs) != 1 || final.Runs[0].ID == uuid.Nil || strings.Contains(result, "runRefs") {
		t.Fatalf("native Investigation must expose only stable evidence references: %s", result)
	}
	runID := final.Runs[0].ID
	path := plane.base(surfaceOrg) + "/tool-runs/" + runID.String() + "/observation"
	status, body := plane.call(t, http.MethodGet, path, nil)
	var retained struct {
		InvestigationID                                 string
		Ordinal                                         int
		ObservationText                                 *string
		Outcome                                         string
		WindowApplied                                   *bool
		IntegrationID, IntegrationName, IntegrationType string
	}
	if status != http.StatusOK {
		t.Fatalf("reading retained observation = %d: %s", status, body)
	}
	decodeInto(t, body, &retained)
	var serialized map[string]any
	decodeInto(t, body, &serialized)
	if err := serializedSchema(t, "ToolRunObservation").Validate(serialized); err != nil {
		t.Fatalf("retained observation violates OpenAPI: %v", err)
	}
	observed := <-model.observation
	if retained.InvestigationID != turn || retained.Ordinal != 0 || retained.ObservationText == nil ||
		*retained.ObservationText != observed.GetOutputText() || observed.ID != "channels" ||
		retained.Outcome != "succeeded" ||
		retained.WindowApplied == nil || *retained.WindowApplied {
		t.Fatalf("retained observation differs from correlated provider input: %s", body)
	}
	var detail struct{ Runs []map[string]json.RawMessage }
	decodeInto(t, result, &detail)
	if _, present := detail.Runs[0]["observationText"]; present {
		t.Fatal("ordinary Investigation detail contains retained observation text")
	}
	if code, _ := plane.call(t, http.MethodGet, strings.Replace(path, runID.String(), "bad", 1), nil); code != http.StatusBadRequest {
		t.Fatalf("invalid Tool Run ID returned %d", code)
	}
	if code, _ := plane.call(t, http.MethodGet, strings.Replace(path, runID.String(), uuid.NewString(), 1), nil); code != http.StatusNotFound {
		t.Fatalf("absent run returned %d", code)
	}
	legacyPath := plane.base(surfaceOrg) + "/investigations/" + turn + "/runs/1/observation"
	if code, _ := plane.call(t, http.MethodGet, legacyPath, nil); code != http.StatusNotFound {
		t.Fatalf("native Tool Run was assigned a synthetic ordinal: %d", code)
	}
	restore := plane.switchOrganization(t, neighbourOrg)
	code, _ := plane.call(t, http.MethodGet, path, nil)
	restore()
	if code != http.StatusNotFound {
		t.Fatalf("foreign observation returned %d", code)
	}
	{
		if retained.IntegrationName != "Operator testimony" || retained.IntegrationType != "slack" || retained.IntegrationID == "" {
			t.Fatalf("observation lost current Integration attribution: %s", body)
		}
		code, renamed := plane.call(t, http.MethodPatch, plane.base(surfaceOrg)+"/integrations/"+retained.IntegrationID, map[string]any{"name": "Renamed source"})
		if code != http.StatusOK {
			t.Fatalf("rename = %d: %s", code, renamed)
		}
		_, renamed = plane.call(t, http.MethodGet, path, nil)
		decodeInto(t, renamed, &retained)
		if retained.IntegrationName != "Renamed source" || *retained.ObservationText != observed.GetOutputText() {
			t.Fatal("Integration rename rewrote the retained observation or kept stale display information")
		}
	}
	database, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	for _, role := range []string{"editor", "viewer", "admin"} {
		if _, err := database.Exec(context.Background(), `UPDATE organization_membership SET role=$2 WHERE org_id=$1`, surfaceOrg, role); err != nil {
			t.Fatal(err)
		}
		code, response := plane.call(t, http.MethodGet, path, nil)
		want := http.StatusOK
		if role == "viewer" {
			want = http.StatusForbidden
		}
		if code != want {
			t.Fatalf("%s observation read = %d: %s", role, code, response)
		}
		if code, response := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn, nil); code != http.StatusOK || strings.Contains(response, "observationText") {
			t.Fatalf("%s ordinary detail changed: %d %s", role, code, response)
		}
	}
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	seen := make(map[string]bool)
	for _, event := range assertSerializedEvents(t, body) {
		kind := event["type"].(string)
		seen[kind] = true
		if kind == "tool_started" || kind == "tool_completed" {
			payload := event["payload"].(map[string]any)
			if payload["toolRunId"] != runID.String() || payload["ordinal"] != nil {
				t.Fatalf("Tool event does not use its durable UUID: %v", payload)
			}
		}
		if kind == "started" {
			if event["payload"].(map[string]any)["text"] != "Reviewing this request and available evidence sources." {
				t.Fatal("Started event does not describe the initial phase")
			}
			event["payload"] = map[string]any{}
			if err := eventSchema(t).Validate(event); err != nil {
				t.Fatalf("legacy empty Started payload is no longer readable: %v", err)
			}
		}
		if kind == "concluded" && event["payload"].(map[string]any)["summary"] != final.Summary {
			t.Fatal("event answer differs from its canonical result")
		}
	}
	if len(seen) != len(want) {
		t.Errorf("event types = %v, want %v", seen, want)
	}
	for _, kind := range want {
		if !seen[kind] {
			t.Errorf("no %s event validated", kind)
		}
	}
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations/"+conversationID+"/messages",
		map[string]any{"message": "Which channel did you find?"})
	if status != http.StatusAccepted {
		t.Fatalf("follow-up admission = %d: %s", status, body)
	}
	var followUp struct {
		Turn struct {
			ID string `json:"investigationId"`
		} `json:"turn"`
	}
	decodeInto(t, body, &followUp)
	if got := plane.awaitInvestigation(t, followUp.Turn.ID); !strings.Contains(got, `"status":"concluded"`) ||
		!strings.Contains(got, `"evidenceRefs":[{"toolRunId":`) {
		t.Fatalf("follow-up lost its earlier Tool Run citation: %s", got)
	}
}

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

func TestCompletedSnapshotKeepsToolRunProvenance(t *testing.T) {
	model := &eventContractModel{tool: "slack_list_channels"}
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
	snapshots := assertSerializedSnapshots(t, body)
	if len(snapshots) != 1 || snapshots[0]["status"] != "completed" ||
		snapshots[0]["conclusionUrl"] != "/api/v1/investigations/"+turn {
		t.Fatalf("terminal snapshot = %s", body)
	}
	tools := snapshots[0]["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["toolRunId"] != runID.String() ||
		tools[0].(map[string]any)["status"] != "succeeded" {
		t.Fatalf("Tool activity did not use the committed UUID: %s", body)
	}
	if strings.Contains(body, retained.IntegrationName) || strings.Contains(body, "incidents") {
		t.Fatalf("snapshot exposed Integration or observation content: %s", body)
	}
	var legacyEvents int
	if err := database.QueryRow(t.Context(), `SELECT count(*) FROM investigation_event
		WHERE org_id = $1 AND investigation_id = $2`, surfaceOrg, turn).Scan(&legacyEvents); err != nil || legacyEvents != 0 {
		t.Fatalf("web execution wrote %d redundant legacy events: %v", legacyEvents, err)
	}
	followUp := plane.seedLegacyTurn(t, conversationID, "Which channel did you find?")
	if got := plane.awaitInvestigation(t, followUp); !strings.Contains(got, `"status":"concluded"`) ||
		!strings.Contains(got, `"evidenceRefs":[{"toolRunId":`) {
		t.Fatalf("follow-up lost its earlier Tool Run citation: %s", got)
	}
}

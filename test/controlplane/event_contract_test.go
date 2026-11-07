package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

type eventContractModel struct {
	calls       int
	tool        string
	observation chan modelagent.ToolResultTurn
}

func (m *eventContractModel) Complete(ctx context.Context, prompt modelagent.Prompt) (modelagent.Completion, error) {
	m.calls++
	if m.calls == 1 {
		return modelagent.Completion{Stop: modelagent.StopToolUse, ToolCalls: []modelagent.CompletionCall{{
			ID: "channels", Name: m.tool,
			Arguments: json.RawMessage(`{"purpose":"Find the incident channel","input":{}}`),
		}}}, nil
	}
	if len(prompt.Turns) > 0 && len(prompt.Turns[0].ToolResults) > 0 {
		m.observation <- prompt.Turns[0].ToolResults[0]
	}
	completion, err := (concludingModel{}).Complete(ctx, prompt)
	if err != nil {
		return completion, err
	}
	var document map[string]any
	if err := json.Unmarshal(completion.ToolCalls[0].Arguments, &document); err != nil {
		return modelagent.Completion{}, err
	}
	document["summary"] = "line one\n" + strings.Repeat("界", 4087)
	completion.ToolCalls[0].Arguments, err = json.Marshal(document)
	return completion, err
}

func TestAvailableToolAndAnswerEventsMatchVersionTwoSchema(t *testing.T) {
	assertModelEventSchema(t, &eventContractModel{tool: "slack_list_channels"},
		[]string{"started", "tool_started", "tool_completed", "concluded"})
}

func TestUnavailableToolCompletionMatchesVersionTwoSchemaWithoutAStart(t *testing.T) {
	assertModelEventSchema(t, &eventContractModel{tool: "unavailable.read"},
		[]string{"started", "tool_completed", "concluded"})
}

func assertModelEventSchema(t *testing.T, model *eventContractModel, want []string) {
	t.Helper()
	model.observation = make(chan modelagent.ToolResultTurn, 1)
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.channels = `{"ok":true,"channels":[{"id":"C0INCIDENT","name":"incidents"}]}`
	address := freeAddress(t)
	var dsn string
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		dsn = cfg.DatabaseDSN
		cfg.HTTPListenAddress = address
		cfg.SetupEnabled = true
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Completer: model, SlackAPIURL: vendor.URL})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address, dsn: dsn}
	if status, body := plane.createSlack(t, "Operator testimony", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("create Slack = %d: %s", status, body)
	}
	_, turn := plane.openConversation(t, "event schema", "investigate checkout")
	result := plane.awaitInvestigation(t, turn)
	var final struct {
		Summary string `json:"summary"`
	}
	decodeInto(t, result, &final)
	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/investigations/"+turn+"/runs/1/observation", nil)
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
	failed := model.tool == "unavailable.read"
	observed := <-model.observation
	if retained.InvestigationID != turn || retained.Ordinal != 1 || retained.ObservationText == nil ||
		*retained.ObservationText != observed.Content || observed.CallID != "channels" ||
		observed.IsError != failed || (retained.Outcome == "failed") != failed ||
		retained.WindowApplied == nil || *retained.WindowApplied {
		t.Fatalf("retained observation differs from correlated provider input: %s", body)
	}
	var detail struct{ Runs []map[string]json.RawMessage }
	decodeInto(t, result, &detail)
	if _, present := detail.Runs[0]["observationText"]; present {
		t.Fatal("ordinary Investigation detail contains retained observation text")
	}
	path := plane.base(surfaceOrg) + "/investigations/" + turn + "/runs/1/observation"
	for _, ordinal := range []string{"0", "-1", "bad", "2147483648"} {
		if code, _ := plane.call(t, http.MethodGet, strings.Replace(path, "/runs/1/", "/runs/"+ordinal+"/", 1), nil); code != http.StatusBadRequest {
			t.Fatalf("invalid ordinal %s returned %d", ordinal, code)
		}
	}
	if code, _ := plane.call(t, http.MethodGet, strings.Replace(path, "/runs/1/", "/runs/2/", 1), nil); code != http.StatusNotFound {
		t.Fatalf("absent run returned %d", code)
	}
	restore := plane.switchOrganization(t, neighbourOrg)
	code, _ := plane.call(t, http.MethodGet, path, nil)
	restore()
	if code != http.StatusNotFound {
		t.Fatalf("foreign observation returned %d", code)
	}
	if !failed {
		if retained.IntegrationName != "Operator testimony" || retained.IntegrationType != "slack" || retained.IntegrationID == "" {
			t.Fatalf("observation lost current Integration attribution: %s", body)
		}
		code, renamed := plane.call(t, http.MethodPatch, plane.base(surfaceOrg)+"/integrations/"+retained.IntegrationID, map[string]any{"name": "Renamed source"})
		if code != http.StatusOK {
			t.Fatalf("rename = %d: %s", code, renamed)
		}
		_, renamed = plane.call(t, http.MethodGet, path, nil)
		decodeInto(t, renamed, &retained)
		if retained.IntegrationName != "Renamed source" || *retained.ObservationText != observed.Content {
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
}

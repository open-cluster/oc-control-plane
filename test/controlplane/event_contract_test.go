package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

type eventContractModel struct {
	calls int
	tool  string
}

func (m *eventContractModel) Complete(ctx context.Context, prompt modelagent.Prompt) (modelagent.Completion, error) {
	m.calls++
	if m.calls == 1 {
		return modelagent.Completion{Stop: modelagent.StopToolUse, ToolCalls: []modelagent.CompletionCall{{
			ID: "channels", Name: m.tool,
			Arguments: json.RawMessage(`{"purpose":"Find the incident channel","input":{}}`),
		}}}, nil
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
	assertModelEventSchema(t, &eventContractModel{tool: "slack.list_channels"},
		[]string{"started", "tool_started", "tool_completed", "concluded"})
}

func TestUnavailableToolCompletionMatchesVersionTwoSchemaWithoutAStart(t *testing.T) {
	assertModelEventSchema(t, &eventContractModel{tool: "unavailable.read"},
		[]string{"started", "tool_completed", "concluded"})
}

func assertModelEventSchema(t *testing.T, model *eventContractModel, want []string) {
	t.Helper()
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.channels = `{"ok":true,"channels":[{"id":"C0INCIDENT","name":"incidents"}]}`
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Completer: model, SlackAPIURL: vendor.URL})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	if status, body := plane.createSlack(t, "Operator testimony", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("create Slack = %d: %s", status, body)
	}
	_, turn := plane.openConversation(t, "event schema", "investigate checkout")
	result := plane.awaitInvestigation(t, turn)
	var final struct {
		Summary string `json:"summary"`
	}
	decodeInto(t, result, &final)
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	seen := make(map[string]bool)
	for _, event := range assertSerializedEvents(t, body) {
		kind := event["type"].(string)
		seen[kind] = true
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

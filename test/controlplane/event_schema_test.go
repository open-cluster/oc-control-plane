package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

type failedEventModel struct{}

func (failedEventModel) Complete(context.Context, modelagent.Prompt) (modelagent.Completion, error) {
	return modelagent.Completion{}, errors.New("provider unavailable")
}

type liveProgressEventModel struct {
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
	thirdStarted chan struct{}
}

func (m *liveProgressEventModel) Complete(ctx context.Context, _ modelagent.Prompt) (modelagent.Completion, error) {
	m.calls++
	switch m.calls {
	case 1:
		close(m.firstStarted)
		select {
		case <-m.releaseFirst:
		case <-ctx.Done():
			return modelagent.Completion{}, ctx.Err()
		}
	case 3:
		close(m.thirdStarted)
		<-ctx.Done()
		return modelagent.Completion{}, ctx.Err()
	}
	calls := []modelagent.CompletionCall{{
		ID: fmt.Sprintf("read-%d", m.calls), Name: "slack_list_channels",
		Arguments: json.RawMessage(`{"purpose":"Read evidence","input":{}}`),
	}}
	if m.calls == 2 {
		duplicate := calls[0]
		duplicate.ID = "duplicate"
		calls = append(calls, duplicate)
	}
	return modelagent.Completion{Stop: modelagent.StopToolUse, ToolCalls: calls}, nil
}

func TestLiveProgressEventMatchesSerializedSchema(t *testing.T) {
	model := &liveProgressEventModel{
		firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
		thirdStarted: make(chan struct{}),
	}
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.channels = `{"ok":true,"channels":[{"id":"C0INCIDENT","name":"incidents"}]}`
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Completer: model, SlackAPIURL: vendor.URL})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	if status, body := plane.createSlack(t, "Operator testimony", "xoxb-good-token-1234"); status != http.StatusCreated {
		t.Fatalf("create Slack = %d: %s", status, body)
	}
	_, turn := plane.openConversation(t, "live progress schema", "investigate checkout")
	select {
	case <-model.firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the model did not start")
	}
	response := openEventStream(t, plane, turn, "")
	defer func() { _ = response.Body.Close() }()
	close(model.releaseFirst)

	schema := eventSchema(t)
	scanner := bufio.NewScanner(response.Body)
	sawProgress := false
	sawCompletion := false
	var seen []string
	for scanner.Scan() {
		data, present := strings.CutPrefix(scanner.Text(), "data: ")
		if !present {
			continue
		}
		event := decodeSerializedEvent(t, schema, data)
		seen = append(seen, event["type"].(string))
		if event["type"] == "tool_completed" {
			sawCompletion = true
			status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn, nil)
			if status != http.StatusOK {
				t.Fatalf("Tool Run detail = %d: %s", status, body)
			}
			var detail struct {
				Runs []struct {
					Ordinal int `json:"ordinal"`
				} `json:"runs"`
			}
			decodeInto(t, body, &detail)
			ordinal := int(event["payload"].(map[string]any)["ordinal"].(float64))
			if len(detail.Runs) < ordinal || detail.Runs[ordinal-1].Ordinal != ordinal {
				t.Fatal("live completion has no committed Tool Run detail")
			}
			status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+
				"/runs/"+fmt.Sprint(ordinal)+"/observation", nil)
			var retained struct{ ObservationText *string }
			decodeInto(t, body, &retained)
			if status != http.StatusOK || retained.ObservationText == nil {
				t.Fatal("live completion has no committed prepared observation")
			}
		}
		if event["type"] == "progress" {
			if !sawCompletion {
				t.Fatal("duplicate progress preceded the completed read")
			}
			sawProgress = true
			break
		}
	}
	if !sawProgress {
		t.Fatalf("live stream ended before progress; events=%v: %v", seen, scanner.Err())
	}
	select {
	case <-model.thirdStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the live stream did not observe progress before the next model exchange")
	}
	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
}

func TestFailedEventMatchesSerializedSchema(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Completer: failedEventModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	_, turn := plane.openConversation(t, "failure schema", "investigate checkout")
	plane.awaitInvestigation(t, turn)
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	events := assertSerializedEvents(t, body)
	if events[len(events)-1]["type"] != "failed" {
		t.Fatal("no failed ending validated")
	}
}

func TestCancelledEventMatchesSerializedSchema(t *testing.T) {
	plane, _ := agentPlane(t, &blockingAgentMain{})
	_, turn := plane.openConversation(t, "cancel schema", "investigate checkout")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	assertSerializedEvents(t, body)
}

func assertSerializedEvents(t *testing.T, stream string) []map[string]any {
	t.Helper()
	schema := eventSchema(t)
	var events []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		data, present := strings.CutPrefix(line, "data: ")
		if !present {
			continue
		}
		events = append(events, decodeSerializedEvent(t, schema, data))
	}
	if len(events) == 0 {
		t.Fatal("no serialized events were validated")
	}
	return events
}

func decodeSerializedEvent(t *testing.T, schema *jsonschema.Schema, data string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(event); err != nil {
		t.Errorf("serialized %v event violates OpenAPI: %v", event["type"], err)
	}
	return event
}

func eventSchema(t *testing.T) *jsonschema.Schema {
	return serializedSchema(t, "InvestigationEvent")
}

func serializedSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	if err := compiler.AddResource("openapi.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("openapi.json#/components/schemas/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

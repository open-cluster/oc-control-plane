package controlplane

import (
	"bufio"
	"context"
	"crypto/sha256"
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
	return modelagent.Completion{Stop: modelagent.StopToolUse, ToolCalls: []modelagent.CompletionCall{{
		ID: fmt.Sprintf("unavailable-%d", m.calls), Name: "unavailable.read",
		Arguments: json.RawMessage(`{"purpose":"Read evidence","input":{}}`),
	}}}, nil
}

func TestLiveProgressEventMatchesSerializedSchema(t *testing.T) {
	model := &liveProgressEventModel{
		firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
		thirdStarted: make(chan struct{}),
	}
	vendor := newVendorFake(t, "xoxb-good-token-1234")
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
	var seen []string
	for scanner.Scan() {
		data, present := strings.CutPrefix(scanner.Text(), "data: ")
		if !present {
			continue
		}
		event := decodeSerializedEvent(t, schema, data)
		seen = append(seen, event["type"].(string))
		if event["type"] == "progress" {
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
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "zai", "glm-4.7", "scripted-model-key"
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
	schema, err := compiler.Compile("openapi.json#/components/schemas/InvestigationEvent")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

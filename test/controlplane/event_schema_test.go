package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

type failedEventModel struct{ bufferedModel }

func (failedEventModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	return nil, errors.New("provider unavailable")
}

type liveProgressEventModel struct {
	bufferedModel
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
	nextStarted  chan struct{}
}

func (m *liveProgressEventModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	switch m.calls {
	case 1:
		close(m.firstStarted)
		select {
		case <-m.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		close(m.nextStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	const input = `{"purpose":"Read evidence","input":{"limit":1}}`
	batch := toolResponse("first-read", "slack_list_channels", input)
	batch.Content = append(batch.Content, toolResponse("second-read", "slack_list_channels", input).Content...)
	return batch, nil
}

func TestLiveToolCompletionMatchesSerializedSchema(t *testing.T) {
	model := &liveProgressEventModel{
		firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
		nextStarted: make(chan struct{}),
	}
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.channels = `{"ok":true,"channels":[{"id":"C0INCIDENT","name":"incidents"},{"id":"C0OPS","name":"operations"}]}`
	releaseSlow := make(chan struct{})
	var reads atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conversations.list" && reads.Add(1) == 1 {
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
			return
		}
		vendor.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(provider.Close)
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Model: model, SlackAPIURL: provider.URL})
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
	response := openEventStream(t, plane, turn)
	defer func() { _ = response.Body.Close() }()
	close(model.releaseFirst)

	schema := snapshotSchema(t)
	scanner := bufio.NewScanner(response.Body)
	var fastID string
	var failed bool
	for scanner.Scan() {
		data, present := strings.CutPrefix(scanner.Text(), "data: ")
		if !present {
			continue
		}
		snapshot := decodeSerializedSnapshot(t, schema, data)
		if snapshot["draftText"] != nil {
			t.Fatal("Investigation published a provisional narrative")
		}
		activityRows := snapshot["tools"].([]any)
		for _, tool := range activityRows {
			activity := tool.(map[string]any)
			id := activity["toolRunId"].(string)
			if activity["status"] == "failed" {
				failed = true
			}
			if activity["status"] != "succeeded" || fastID != "" {
				continue
			}
			if len(activityRows) != 2 || activityRows[0].(map[string]any)["status"] != "running" || activity["truncated"] != true {
				t.Fatalf("snapshot lost the running peer or fast Tool truncation: %v", activityRows)
			}
			select {
			case <-model.nextStarted:
				t.Fatal("native model advanced before the held Tool returned")
			default:
			}
			status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn, nil)
			if status != http.StatusOK {
				t.Fatalf("Tool Run detail = %d: %s", status, body)
			}
			var detail struct {
				Runs []struct {
					ID string `json:"id"`
				} `json:"runs"`
			}
			decodeInto(t, body, &detail)
			if !slices.ContainsFunc(detail.Runs, func(run struct {
				ID string `json:"id"`
			}) bool {
				return run.ID == id
			}) {
				t.Fatal("live completion has no committed Tool Run detail")
			}
			status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/tool-runs/"+id+"/observation", nil)
			var retained struct{ ObservationText *string }
			decodeInto(t, body, &retained)
			if status != http.StatusOK || retained.ObservationText == nil {
				t.Fatal("live completion has no committed prepared observation")
			}
			fastID = id
			close(releaseSlow)
		}
		if failed && fastID != "" {
			break
		}
	}
	if !failed || fastID == "" {
		t.Fatalf("live stream ended before a successful and a failed Tool completion: %v", scanner.Err())
	}
	select {
	case <-model.nextStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the model did not receive the second Tool result")
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
	}, app.Options{Model: failedEventModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	_, turn := plane.openConversation(t, "failure schema", "investigate checkout")
	plane.awaitInvestigation(t, turn)
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	events := assertSerializedSnapshots(t, body)
	if events[len(events)-1]["status"] != "failed" {
		t.Fatal("no failed ending validated")
	}
}

func TestCancelledEventMatchesSerializedSchema(t *testing.T) {
	plane, _ := agentPlane(t, &blockingModel{})
	_, turn := plane.openConversation(t, "cancel schema", "investigate checkout")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	assertSerializedSnapshots(t, body)
}

func assertSerializedSnapshots(t *testing.T, stream string) []map[string]any {
	t.Helper()
	schema := snapshotSchema(t)
	var events []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		data, present := strings.CutPrefix(line, "data: ")
		if !present {
			continue
		}
		events = append(events, decodeSerializedSnapshot(t, schema, data))
	}
	if len(events) == 0 {
		t.Fatal("no serialized events were validated")
	}
	return events
}

func decodeSerializedSnapshot(t *testing.T, schema *jsonschema.Schema, data string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(event); err != nil {
		t.Errorf("serialized %v event violates OpenAPI: %v", event["status"], err)
	}
	return event
}

func snapshotSchema(t *testing.T) *jsonschema.Schema {
	return serializedSchema(t, "ExecutionSnapshot")
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

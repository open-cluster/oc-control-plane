package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

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

func TestFailedEventMatchesSerializedSchema(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Model: failedEventModel{}})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
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
	plane, _ := agentPlane(t, &blockingAgentMain{started: make(chan struct{}, 1)})
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
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(event); err != nil {
			t.Errorf("serialized %v event violates OpenAPI: %v", event["type"], err)
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		t.Fatal("no serialized events were validated")
	}
	return events
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

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestPrivateModelReasoningNeverCrossesTheProviderBoundary(t *testing.T) {
	const marker = "private-reasoning-must-never-escape-4f2b"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			writeOpenRouterInvestigationRecap(w, marker)
			return
		}
		document := map[string]any{
			"status": "inconclusive", "summary": "No cause was established.",
			"impact":   map[string]any{"summary": "Impact is not established."},
			"findings": []any{}, "hypotheses": []any{}, "actions": []any{},
			"limitations": []map[string]any{{
				"type": "essential_human_input", "statement": "Operator context is required.",
			}},
		}
		arguments, err := json.Marshal(document)
		if err != nil {
			t.Errorf("encoding conclusion: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chat-confidentiality", "request_id": "req-confidentiality", "model": "z-ai/glm-4.7",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": "", "reasoning": marker,
					"tool_calls": []map[string]any{{
						"id": "report", "type": "function",
						"function": map[string]any{"name": "generate_structured_output", "arguments": string(arguments)},
					}},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
	}))
	t.Cleanup(provider.Close)

	apiAddress := freeAddress(t)
	var dsn string
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		cfg.ModelProvider = "openrouter"
		cfg.ModelName = "z-ai/glm-4.7"
		cfg.ModelAPIKey = "test-provider-key"
		dsn = cfg.DatabaseDSN
	}, app.Options{ModelBaseURL: provider.URL})
	plane := &integrationPlane{controlPlane: running, api: apiAddress,
		intake: apiAddress, dsn: dsn}

	conversation, turn := plane.openConversation(t, "reasoning confidentiality",
		"What can be established from the currently connected sources?")
	if turn == "" {
		t.Fatal("opening the conversation did not create an Investigation")
	}
	final := plane.awaitInvestigation(t, turn)
	if !strings.Contains(final, `"status":"concluded"`) {
		t.Fatalf("the Investigation did not commit a report: %s", final)
	}
	status, transcript := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/conversations/"+conversation, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the conversation = %d: %s", status, transcript)
	}
	if strings.Contains(final, marker) || strings.Contains(transcript, marker) {
		t.Fatal("private reasoning appeared in an operator HTTP response")
	}
	if strings.Contains(plane.logs.String(), marker) {
		t.Fatal("private reasoning appeared in process logs")
	}

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to durable truth: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var matches int
	err = connection.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM investigation
			  WHERE org_id = $2 AND concat_ws(' ', subject, conclusion::text, error) LIKE '%' || $1 || '%') +
			(SELECT count(*) FROM investigation_event
			  WHERE org_id = $2 AND payload::text LIKE '%' || $1 || '%') +
			(SELECT count(*) FROM investigation_tool_run
			  WHERE org_id = $2 AND concat_ws(' ', arguments::text, summary, sources::text, error, purpose, observation_text) LIKE '%' || $1 || '%') +
			(SELECT count(*) FROM conversation_message
			  WHERE org_id = $2 AND text LIKE '%' || $1 || '%') +
			(SELECT count(*) FROM audit_event
			  WHERE org_id = $2 AND detail::text LIKE '%' || $1 || '%')`, marker, surfaceOrg).Scan(&matches)
	if err != nil {
		t.Fatalf("scanning persisted surfaces: %v", err)
	}
	if matches != 0 {
		t.Fatalf("private reasoning appeared in %d persisted rows", matches)
	}
}

func writeOpenRouterInvestigationRecap(w http.ResponseWriter, marker string) {
	_, _ = fmt.Fprintf(w, `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"No cause established.","reasoning":%q},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n", marker)
}

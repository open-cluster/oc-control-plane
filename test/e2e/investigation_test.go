package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func startInvestigationModel(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := capabilityID
		arguments := fmt.Sprintf(`{"namespace":%q,"workloadKind":"Deployment","workloadName":%q,"maxPods":10}`,
			fixtureNamespace, fixtureWorkload)
		if calls.Add(1) > 1 {
			name = "conclude"
			arguments = fmt.Sprintf(`{"status":"verified_cause","summary":"Relay observed the failing workload.","impact":{"status":"partial","current_state":"ongoing","affected_services":[%q],"affected_users":[],"summary":"The workload is unhealthy.","run_refs":[1]},"findings":[{"id":"finding-1","statement":"Relay observed workload %s in namespace %s","kind":"cause","confidence":"confirmed","mechanism":"the unhealthy workload serves the affected requests","run_refs":[1]}],"hypotheses":[],"actions":[],"limitations":[]}`,
				fixtureWorkload,
				fixtureWorkload, fixtureNamespace)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		usage := `{"input_tokens":100,"output_tokens":20}`
		_, _ = fmt.Fprintf(writer, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{"+
			"\"id\":\"msg_e2e\",\"type\":\"message\",\"role\":\"assistant\","+
			"\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,"+
			"\"stop_sequence\":null,\"usage\":%s}}\n\n", usage)
		_, _ = fmt.Fprintf(writer, "event: content_block_start\ndata: {\"type\":\"content_block_start\","+
			"\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,"+
			"\"name\":%q,\"input\":{}}}\n\n", fmt.Sprintf("call-%d", calls.Load()), name)
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(writer, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\","+
			"\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", encoded)
		_, _ = fmt.Fprint(writer, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = fmt.Fprintf(writer, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":"+
			"{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":%s}\n\n", usage)
		_, _ = fmt.Fprint(writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func (h *harness) assertInvestigation(t *testing.T) {
	t.Helper()
	base := "http://" + h.plane.httpAddress + "/api/v1"
	status, body := h.operatorRequest(t, http.MethodPost,
		base+"/integrations/"+h.integration.String()+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying the real Relay integration = %d: %s", status, body)
	}

	incident := uuid.New()
	now := time.Now().UTC()
	_, err := h.truth.pool.Exec(context.Background(), `
		INSERT INTO incident
			(incident_id, org_id, integration_id, grouping_key, grouping_basis,
			 title, status, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, 1, $5, 1, $6, $6)`,
		incident, organization, h.integration, "e2e-investigation", fixtureWorkload, now)
	if err != nil {
		t.Fatalf("creating the investigation incident: %v", err)
	}
	status, body = h.operatorRequest(t, http.MethodPost, base+"/investigations",
		map[string]string{"incidentId": incident.String()})
	if status != http.StatusAccepted {
		t.Fatalf("opening the investigation = %d: %s", status, body)
	}
	var opened struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &opened); err != nil || opened.ID == uuid.Nil {
		t.Fatalf("decoding the opened investigation: %v %s", err, body)
	}

	h.await(t, "an investigation to cite its real Relay read", jobTimeout,
		func(ctx context.Context) (bool, error) {
			var status int16
			var conclusion []byte
			var runs int
			err := h.truth.pool.QueryRow(ctx, `
				SELECT investigation.status, investigation.conclusion,
				       count(tool.ordinal)
				  FROM investigation
				  LEFT JOIN investigation_tool_run tool
				    ON tool.investigation_id = investigation.investigation_id
				   AND tool.org_id = investigation.org_id
				 WHERE investigation.investigation_id = $1 AND investigation.org_id = $2
				 GROUP BY investigation.investigation_id`, opened.ID, organization).
				Scan(&status, &conclusion, &runs)
			if err != nil {
				return false, err
			}
			if status == 3 {
				return false, fmt.Errorf("investigation failed; model calls=%d logs=%s",
					runs, h.plane.logsSinceStart())
			}
			return status == 2 && runs == 1 &&
				bytes.Contains(conclusion, []byte(fixtureWorkload)) &&
				bytes.Contains(conclusion, []byte(`"runRefs": [1]`)), nil
		})
}

func (h *harness) operatorRequest(t *testing.T, method, url string, payload any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encoding operator request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("creating operator request: %v", err)
	}
	request.AddCookie(h.plane.session)
	request.Header.Set("X-OpenCluster-Organization", organization)
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("Origin", "http://"+h.plane.httpAddress)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("sending operator request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading operator response: %v", err)
	}
	return response.StatusCode, body
}

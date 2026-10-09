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
		arguments := fmt.Sprintf(`{"purpose":"Read the affected workload's current state","input":{"namespace":%q,"workloadKind":"Deployment","workloadName":%q,"maxPods":10}}`,
			fixtureNamespace, fixtureWorkload)
		if calls.Add(1) > 1 {
			name = "conclude"
			arguments = fmt.Sprintf(`{"status":"verified_cause","summary":"Relay observed the failing workload.","impact":{"summary":"The workload is unhealthy.","run_refs":[1]},"findings":[{"statement":"Relay observed workload %s in namespace %s","kind":"cause","mechanism":"the unhealthy workload serves the affected requests","run_refs":[1],"evidence_refs":[]}],"hypotheses":[],"actions":[],"limitations":[]}`,
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
	h.await(t, "a verified Relay workload Tool", time.Minute, func(context.Context) (bool, error) {
		status, body := h.apiRequest(t, http.MethodPost,
			base+"/integrations/"+h.integration.String()+"/verify", nil)
		if status != http.StatusOK {
			return false, fmt.Errorf("verifying the real Relay Integration = %d: %s", status, body)
		}
		var verified struct {
			Status           string `json:"status"`
			ToolAvailability []struct {
				Tool      string `json:"tool"`
				Available bool   `json:"available"`
			} `json:"toolAvailability"`
		}
		if err := json.Unmarshal(body, &verified); err != nil {
			return false, err
		}
		for _, tool := range verified.ToolAvailability {
			if verified.Status == "verified" && tool.Tool == capabilityID && tool.Available {
				return true, nil
			}
		}
		return false, fmt.Errorf("Relay workload Tool is unavailable: %s", body)
	})

	incident := uuid.New()
	now := time.Now().UTC()
	_, err := h.truth.pool.Exec(context.Background(), `
		INSERT INTO incident
			(incident_id, org_id, integration_id, grouping_key, grouping_basis,
			 title, status, first_seen_at, last_seen_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, $5, 1, $6, $6, now())`,
		incident, organization, h.integration, "e2e-investigation", fixtureWorkload, now)
	if err != nil {
		t.Fatalf("creating the investigation incident: %v", err)
	}
	status, body := h.apiRequest(t, http.MethodPost, base+"/investigations",
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
	var successfulRuns int
	err = h.truth.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM investigation_tool_run
		WHERE org_id = $1 AND investigation_id = $2 AND integration_id = $3
		  AND ordinal = 1 AND tool = $4 AND outcome = 1`,
		organization, opened.ID, h.integration, capabilityID).Scan(&successfulRuns)
	if err != nil || successfulRuns != 1 {
		t.Fatalf("Investigation must record one successful Relay read: runs=%d err=%v", successfulRuns, err)
	}
	var jobs int
	var job jobRecord
	err = h.truth.pool.QueryRow(context.Background(), `
		SELECT count(*), max(status), (array_agg(result))[1] FROM relay_job
		WHERE org_id = $1 AND investigation_id = $2 AND integration_id = $3 AND capability_id = $4`,
		organization, opened.ID, h.integration, capabilityID).Scan(&jobs, &job.Status, &job.Result)
	if err != nil || jobs != 1 || job.Status != jobSucceeded {
		t.Fatalf("Investigation must have one succeeded Relay Job: jobs=%d status=%s err=%v", jobs, job.Status, err)
	}
	assertReadTheFixture(t, decodeResult(t, job.Result))
}

func (h *harness) apiRequest(t *testing.T, method, url string, payload any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encoding API request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("creating API request: %v", err)
	}
	request.AddCookie(h.plane.session)
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("Origin", "http://"+h.plane.httpAddress)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("sending API request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading API response: %v", err)
	}
	return response.StatusCode, body
}

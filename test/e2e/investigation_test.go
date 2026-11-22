package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

const workloadToolName = "kubernetes_workload_runtime"
const workloadCallID = "read-workload"

func startInvestigationModel(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		run, observed, stream, err := workloadResult(request.Body)
		if err != nil {
			t.Error(err)
			http.Error(writer, "invalid workload Tool result", http.StatusBadRequest)
			return
		}
		name, callID := workloadToolName, workloadCallID
		arguments := fmt.Sprintf(`{"purpose":"Read the affected workload's current state","input":{"namespace":%q,"workloadKind":"Deployment","workloadName":%q,"maxPods":10}}`,
			fixtureNamespace, fixtureWorkload)
		if observed {
			name, callID = "generate_structured_output", "report"
			arguments = `{"status":"inconclusive","summary":"Workload evidence is unavailable.",
				"impact":{"summary":"Unknown"},"findings":[],"hypotheses":[],"actions":[],"limitations":[]}`
			if run != uuid.Nil {
				arguments = fmt.Sprintf(`{"status":"supported_explanation","summary":"Relay observed the failing workload.",
					"impact":{"summary":"The workload is unhealthy.","evidenceRefs":[{"toolRunId":%q}]},
					"findings":[{"statement":%q,"kind":"cause","mechanism":"the unhealthy workload serves the affected requests",
					"evidenceRefs":[{"toolRunId":%q}]}],"hypotheses":[],"actions":[],"limitations":[]}`,
					run, "Relay observed workload "+fixtureWorkload+" in namespace "+fixtureNamespace, run)
			}
		}
		if !stream {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"id":"msg_report","type":"message","role":"assistant","model":"claude-sonnet-5",
				"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],
				"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20}}`, callID, name, arguments)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		usage := `{"input_tokens":100,"output_tokens":20}`
		_, _ = fmt.Fprintf(writer, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{"+
			"\"id\":\"msg_e2e\",\"type\":\"message\",\"role\":\"assistant\","+
			"\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,"+
			"\"stop_sequence\":null,\"usage\":%s}}\n\n", usage)
		if observed {
			_, _ = fmt.Fprint(writer, "event: content_block_start\ndata: {\"type\":\"content_block_start\","+
				"\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"The workload read completed.\"}}\n\n")
			_, _ = fmt.Fprint(writer, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = fmt.Fprintf(writer, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":"+
				"{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":%s}\n\n", usage)
			_, _ = fmt.Fprint(writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = fmt.Fprintf(writer, "event: content_block_start\ndata: {\"type\":\"content_block_start\","+
			"\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,"+
			"\"name\":%q,\"input\":{}}}\n\n", callID, name)
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

func workloadResult(body io.Reader) (uuid.UUID, bool, bool, error) {
	var request struct {
		Stream   bool
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
				IsError   bool   `json:"is_error"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		return uuid.Nil, false, false, fmt.Errorf("decoding the Anthropic request: %w", err)
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type != "tool_result" || block.ToolUseID != workloadCallID {
				continue
			}
			if block.IsError {
				return uuid.Nil, true, request.Stream, nil
			}
			observation, hasPrefix := strings.CutPrefix(block.Content, "[Tool Run ")
			id, _, hasEnd := strings.Cut(observation, "]")
			if !hasPrefix || !hasEnd {
				return uuid.Nil, true, request.Stream, nil
			}
			run, err := uuid.Parse(id)
			return run, true, request.Stream, err
		}
	}
	return uuid.Nil, false, request.Stream, nil
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
			if verified.Status == "verified" && tool.Tool == workloadToolName && tool.Available {
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

	var report investigation.Conclusion
	h.await(t, "an investigation to cite its real Relay read", jobTimeout,
		func(ctx context.Context) (bool, error) {
			var status investigation.Status
			var conclusion []byte
			var runs int
			err := h.truth.pool.QueryRow(ctx, `
				SELECT investigation.status, investigation.conclusion,
				       count(tool.tool_run_id)
				  FROM investigation
				  JOIN agent_execution execution
				    ON execution.org_id = investigation.org_id
				   AND execution.investigation_id = investigation.investigation_id
				  LEFT JOIN investigation_tool_run tool
				    ON tool.org_id = execution.org_id AND tool.execution_id = execution.execution_id
				 WHERE investigation.investigation_id = $1 AND investigation.org_id = $2
				 GROUP BY investigation.investigation_id`, opened.ID, organization).
				Scan(&status, &conclusion, &runs)
			if err != nil {
				return false, err
			}
			if status == investigation.StatusFailed || status == investigation.StatusCancelled {
				t.Fatalf("Investigation ended %s after %d Tool Runs\n%s", status, runs, h.diagnostics())
			}
			if status != investigation.StatusConcluded {
				return false, nil
			}
			if err := json.Unmarshal(conclusion, &report); err != nil || runs != 1 {
				t.Fatalf("decoding Conclusion with one Tool Run: runs=%d err=%v", runs, err)
			}
			return true, nil
		})
	if len(report.Findings) != 1 || len(report.Findings[0].EvidenceRefs) != 1 ||
		!strings.Contains(report.Findings[0].Statement, fixtureWorkload) {
		t.Fatalf("Conclusion must cite one Tool Run for the workload: findings=%+v", report.Findings)
	}
	cited := report.Findings[0].EvidenceRefs[0].ToolRunID
	var successfulRuns int
	err = h.truth.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM investigation_tool_run tool
		JOIN agent_execution execution ON execution.org_id = tool.org_id AND execution.execution_id = tool.execution_id
		WHERE tool.org_id = $1 AND execution.investigation_id = $2 AND tool.integration_id = $3
		  AND tool.tool = $4 AND tool.outcome = $5 AND tool.tool_run_id = $6`,
		organization, opened.ID, h.integration, workloadToolName, investigation.RunSucceeded, cited).Scan(&successfulRuns)
	if err != nil || successfulRuns != 1 {
		t.Fatalf("Investigation must record one successful Relay read: runs=%d err=%v", successfulRuns, err)
	}
	var jobs int
	var job jobRecord
	err = h.truth.pool.QueryRow(context.Background(), `
		SELECT count(*), max(status), (array_agg(result))[1] FROM relay_job
		WHERE org_id = $1 AND tool_run_id = $2 AND integration_id = $3 AND capability_id = $4`,
		organization, cited, h.integration, capabilityID).Scan(&jobs, &job.Status, &job.Result)
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

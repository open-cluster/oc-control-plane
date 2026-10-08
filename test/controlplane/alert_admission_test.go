package controlplane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

func TestAlertPendingCapacityReturnsRetryableServiceUnavailable(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	first := string(alertmanagerPayload("first", "first"))
	if status := plane.deliver(t, intakeSecret, first); status != http.StatusAccepted {
		t.Fatalf("first delivery=%d", status)
	}
	awaitAlertAgent(t, plane)
	second := string(alertmanagerPayload("second", "second"))
	if status := plane.deliver(t, intakeSecret, second); status != http.StatusAccepted {
		t.Fatalf("second delivery=%d", status)
	}
	status, headers, body := deliverAlertAdmission(t, plane,
		string(alertmanagerPayload("third", "third")))
	if status != http.StatusServiceUnavailable || headers.Get("Retry-After") == "" {
		t.Fatalf("capacity refusal=%d Retry-After=%q: %s", status, headers.Get("Retry-After"), body)
	}
	if status := plane.deliver(t, intakeSecret, first); status != http.StatusOK {
		t.Fatalf("exact accepted duplicate at full capacity=%d", status)
	}
	if events := plane.alertEvents(t); len(events) != 2 {
		t.Fatalf("refused delivery retained %d Alert Events, want the two accepted deliveries", len(events))
	}
}

func TestOversizedAlertBatchReturnsPermanentBadRequest(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	body := `{"alerts":[
		{"status":"firing","fingerprint":"one","labels":{"alertname":"untrusted-one"},"startsAt":"2026-09-29T10:00:00Z"},
		{"status":"firing","fingerprint":"two","labels":{"alertname":"untrusted-two"},"startsAt":"2026-09-29T10:00:00Z"}]}`
	status, headers, response := deliverAlertAdmission(t, plane, body)
	if status != http.StatusBadRequest || headers.Get("Retry-After") != "" || strings.Contains(response, "untrusted-") {
		t.Fatalf("permanent refusal=%d Retry-After=%q: %s", status, headers.Get("Retry-After"), response)
	}
	if events := plane.alertEvents(t); len(events) != 0 {
		t.Fatalf("oversized delivery left %d Alert Events", len(events))
	}
}

func TestAlertAdmissionEmitsDistinctOperationalSignals(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	acceptedBody := string(alertmanagerPayload("accepted", "accepted"))
	if status := plane.deliver(t, intakeSecret, acceptedBody); status != http.StatusAccepted {
		t.Fatalf("accepted delivery=%d", status)
	}
	if status := plane.deliver(t, intakeSecret, acceptedBody); status != http.StatusOK {
		t.Fatalf("duplicate delivery=%d", status)
	}
	awaitAlertAgent(t, plane)
	if status := plane.deliver(t, intakeSecret,
		string(alertmanagerPayload("waiting", "waiting"))); status != http.StatusAccepted {
		t.Fatalf("waiting delivery=%d", status)
	}
	if status := plane.deliver(t, intakeSecret,
		string(alertmanagerPayload("capacity", "capacity"))); status != http.StatusServiceUnavailable {
		t.Fatalf("capacity refusal=%d", status)
	}
	oversized := `{"alerts":[
		{"status":"firing","fingerprint":"one","labels":{"alertname":"one"},"startsAt":"2026-09-29T10:00:00Z"},
		{"status":"firing","fingerprint":"two","labels":{"alertname":"two"},"startsAt":"2026-09-29T10:00:00Z"}]}`
	if status := plane.deliver(t, intakeSecret, oversized); status != http.StatusBadRequest {
		t.Fatalf("permanent batch refusal=%d", status)
	}

	status, metrics := plane.get(t, "/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", status, metrics)
	}
	hasMetric := func(name, value string, labels ...string) bool {
		for line := range strings.Lines(metrics) {
			if !strings.HasPrefix(line, name) ||
				!strings.HasSuffix(strings.TrimSpace(line), " "+value) {
				continue
			}
			matched := true
			for _, label := range labels {
				matched = matched && strings.Contains(line, label)
			}
			if matched {
				return true
			}
		}
		return false
	}
	for _, signal := range []struct {
		name   string
		value  string
		labels []string
	}{
		{"oc_webhooks_requests_total", "2", []string{`surface="alert"`, `result="accepted"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="duplicate"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="rate_limited"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="rejected"`}},
		{"oc_webhooks_alert_events_total", "2", nil},
	} {
		if !hasMetric(signal.name, signal.value, signal.labels...) {
			t.Errorf("metrics do not expose %s=%s with %v:\n%s",
				signal.name, signal.value, signal.labels, metrics)
		}
	}
}

func TestAlertAdmissionCorrelatesAcceptedAndRefusedRequests(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 2)
	tests := []struct {
		body   string
		status int
	}{
		{string(alertmanagerPayload("correlated", "correlated")), http.StatusAccepted},
		{"{", http.StatusBadRequest},
	}
	for _, test := range tests {
		status, headers, body := deliverAlertAdmission(t, plane, test.body)
		if status != test.status {
			t.Fatalf("delivery status=%d, want %d: %s", status, test.status, body)
		}
		requestID := headers.Get("X-Request-Id")
		if len(requestID) != 32 {
			t.Fatalf("delivery request ID=%q", requestID)
		}
		var matched bool
		for _, entry := range plane.logs.logLines(t) {
			if entry["msg"] != "request served" || entry["request_id"] != requestID {
				continue
			}
			matched = true
			if entry["status"] != float64(test.status) {
				t.Errorf("request %s logged status=%v", requestID, entry["status"])
			}
			if traceID, ok := entry["trace_id"].(string); !ok || strings.Trim(traceID, "0") == "" {
				t.Errorf("request %s logged trace_id=%v", requestID, entry["trace_id"])
			}
		}
		if !matched {
			t.Errorf("no request log matched response request ID %q", requestID)
		}
	}
}

func TestAcceptedAlertInvestigationRemainsDurableWhenRestartedWithoutAgent(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	if status := plane.deliver(t, intakeSecret, string(alertmanagerPayload("restart", "restart"))); status != http.StatusAccepted {
		t.Fatalf("delivery before restart=%d", status)
	}
	awaitAlertAgent(t, plane)
	plane.shutdown()
	startControlPlane(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.MaxPendingInvestigations = 1
	})
	database, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	var investigations int
	var assigned bool
	if err = database.QueryRow(context.Background(), `SELECT count(*), bool_and(lease_worker <> '')
		FROM investigation WHERE org_id = $1`, intakeOrganization).Scan(&investigations, &assigned); err != nil {
		t.Fatal(err)
	}
	if investigations != 1 || !assigned {
		t.Fatalf("restart without an Agent changed accepted Investigation count=%d assigned=%t",
			investigations, assigned)
	}
}

func startAlertAdmissionIntake(t *testing.T, maximum int) *intakePlane {
	t.Helper()
	var dsn string
	agent := &blockingAgentMain{started: make(chan uuid.UUID, 1)}
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.MaxPendingInvestigations = maximum
		cfg.InvestigationWorkers = 1
		dsn = cfg.DatabaseDSN
	}, app.Options{Agent: agent})
	return &intakePlane{controlPlane: plane, address: listeningAddress(t, plane, ""),
		integration: configureIntegration(t, dsn, intakeOrganization, intakeSecret), dsn: dsn,
		agentStarted: agent.started}
}

func awaitAlertAgent(t *testing.T, plane *intakePlane) {
	t.Helper()
	select {
	case <-plane.agentStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic Investigation was not claimed")
	}
}

func deliverAlertAdmission(t *testing.T, plane *intakePlane, body string) (int, http.Header, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("http://%s/webhooks/v1/integrations/%s/alert-events", plane.address, plane.integration),
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(webhooks.TokenHeader, intakeSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, string(content)
}

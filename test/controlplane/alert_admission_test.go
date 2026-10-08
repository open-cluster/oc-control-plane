package controlplane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

func TestAlertAdmissionWithoutAgentKeepsIncidentIntakeAvailable(t *testing.T) {
	plane := startAlertAdmissionIntakeWithOptions(t, 1, app.Options{})
	if status := plane.deliver(t, intakeSecret,
		string(alertmanagerPayload("no-agent", "no-agent"))); status != http.StatusAccepted {
		t.Fatalf("delivery without an Agent=%d", status)
	}

	ctx := context.Background()
	connection, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	var incidents, investigations int
	if err = connection.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM incident WHERE org_id = $1),
		       (SELECT count(*) FROM investigation WHERE org_id = $1)`,
		intakeOrganization).Scan(&incidents, &investigations); err != nil {
		t.Fatal(err)
	}
	if incidents != 1 || investigations != 0 {
		t.Fatalf("without Agent: incidents=%d investigations=%d, want 1 and 0",
			incidents, investigations)
	}
}

func TestAlertPendingCapacityReturnsRetryableServiceUnavailable(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	first := string(alertmanagerPayload("first", "first"))
	if status := plane.deliver(t, intakeSecret, first); status != http.StatusAccepted {
		t.Fatalf("first delivery=%d", status)
	}
	status, headers, body := deliverAlertAdmission(t, plane,
		string(alertmanagerPayload("second", "second")))
	if status != http.StatusServiceUnavailable || headers.Get("Retry-After") == "" {
		t.Fatalf("capacity refusal=%d Retry-After=%q: %s", status, headers.Get("Retry-After"), body)
	}
	if status := plane.deliver(t, intakeSecret, first); status != http.StatusOK {
		t.Fatalf("exact accepted duplicate at full capacity=%d", status)
	}
	if events := plane.alertEvents(t); len(events) != 1 {
		t.Fatalf("refused delivery retained %d Alert Events, want one from first delivery", len(events))
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
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="accepted"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="duplicate"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="rate_limited"`}},
		{"oc_webhooks_requests_total", "1", []string{`surface="alert"`, `result="rejected"`}},
		{"oc_webhooks_alert_events_total", "1", nil},
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

func TestAcceptedAlertInvestigationRemainsClaimableAfterApplicationRestart(t *testing.T) {
	plane := startAlertAdmissionIntake(t, 1)
	if status := plane.deliver(t, intakeSecret, string(alertmanagerPayload("restart", "restart"))); status != http.StatusAccepted {
		t.Fatalf("delivery before restart=%d", status)
	}
	plane.shutdown()
	startControlPlane(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.MaxPendingInvestigations = 1
	})
	database, err := storage.OpenDatabase(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	org, claimed, found, err := database.ClaimInvestigation(context.Background(),
		investigation.Claim{Worker: "restart-runner", LeaseFor: time.Minute})
	if err != nil || !found || org.String() != intakeOrganization {
		t.Fatalf("claim after restart: org=%s found=%t err=%v", org, found, err)
	}
	if _, err := database.Investigation(context.Background(), org, claimed.ID); err != nil {
		t.Fatalf("reading Investigation after restart: %v", err)
	}
	if _, found, err := database.ClaimSlackMessageWork(context.Background(), "restart-worker", time.Minute); err != nil || found {
		t.Fatalf("alert still depended on webhook job: found=%t err=%v", found, err)
	}
}

func startAlertAdmissionIntake(t *testing.T, maximum int) *intakePlane {
	t.Helper()
	return startAlertAdmissionIntakeWithOptions(t, maximum,
		app.Options{Agent: &blockingAgentMain{}})
}

func startAlertAdmissionIntakeWithOptions(
	t *testing.T, maximum int, options app.Options,
) *intakePlane {
	t.Helper()
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.MaxPendingInvestigations = maximum
		dsn = cfg.DatabaseDSN
	}, options)
	return &intakePlane{controlPlane: plane, address: listeningAddress(t, plane, ""),
		integration: configureIntegration(t, dsn, intakeOrganization, intakeSecret), dsn: dsn}
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

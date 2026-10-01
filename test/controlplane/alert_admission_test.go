package controlplane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

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
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.MaxPendingInvestigations = maximum
		dsn = cfg.DatabaseDSN
	})
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

package e2e

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAcceptedWebhookDeliverySurvivesAbruptProcessTermination(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	integrationID := uuid.New()
	secret := "e2e-generic-webhook-secret-with-sufficient-entropy"
	digest := sha256.Sum256([]byte(secret))
	if _, err := h.truth.pool.Exec(ctx, `
		INSERT INTO integration
			(integration_id, org_id, provider, name, webhook_secret_digest)
		VALUES ($1, $2, 'generic_webhook', 'E2E Generic Webhook', $3)`,
		integrationID, organization, digest[:]); err != nil {
		t.Fatalf("creating generic webhook integration: %v", err)
	}

	body := `{"eventId":"restart-42","status":"firing","title":"Restart proof",` +
		`"severity":"critical","startedAt":"2026-08-28T07:00:00Z",` +
		`"deduplicationKey":"restart-proof"}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+h.plane.httpAddress+"/webhooks/v1/integrations/"+
			integrationID.String()+"/alert-events", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-OpenCluster-Token", secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering canonical alert event: %v", err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery = %d: %s", response.StatusCode, responseBody)
	}

	var deliveryID, investigationID uuid.UUID
	if err = h.truth.pool.QueryRow(ctx, `
		SELECT delivery_id FROM webhook_delivery
		 WHERE org_id = $1 AND integration_id = $2 AND provider_identity = 'restart-42'
		   AND lifecycle_phase = 'firing'`, organization, integrationID).Scan(&deliveryID); err != nil {
		t.Fatalf("202 returned before durable acceptance: %v", err)
	}
	if err := h.truth.pool.QueryRow(ctx, `SELECT investigation.investigation_id
		FROM investigation JOIN incident USING (org_id, incident_id)
		WHERE investigation.org_id = $1 AND incident.integration_id = $2
		  AND investigation.automatic_incident`, organization, integrationID).Scan(&investigationID); err != nil {
		t.Fatalf("202 returned before automatic Investigation creation: %v", err)
	}
	var jobs int
	if err := h.truth.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_job
		WHERE org_id = $1 AND delivery_id = $2`, organization, deliveryID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("accepted alert retained webhook jobs=%d err=%v", jobs, err)
	}

	h.plane.program.kill()
	if err = h.plane.start(ctx, h.plane.spkiPin); err != nil {
		t.Fatalf("restarting the control plane: %v", err)
	}

	var retained bool
	if err := h.truth.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM investigation
		WHERE org_id = $1 AND investigation_id = $2 AND automatic_incident)`, organization, investigationID).
		Scan(&retained); err != nil || !retained {
		t.Fatalf("automatic Investigation lost after abrupt restart: retained=%t err=%v", retained, err)
	}
}

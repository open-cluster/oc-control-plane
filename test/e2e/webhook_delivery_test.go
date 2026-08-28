package e2e

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
			(integration_id, org_id, integration_type_id, name, webhook_secret_digest,
			 webhook_secret_fingerprint, webhook_secret_created_at)
		VALUES ($1, $2, 5, 'E2E Generic Webhook', $3, 'e2e-generic', now())`,
		integrationID, organization, digest[:]); err != nil {
		t.Fatalf("creating generic webhook integration: %v", err)
	}

	blocker, err := h.truth.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err = blocker.Exec(ctx, `LOCK TABLE investigation IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("blocking delivery processing: %v", err)
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

	var deliveryID uuid.UUID
	if err = h.truth.pool.QueryRow(ctx, `
		SELECT delivery_id FROM integration_delivery
		 WHERE integration_id = $1 AND provider_identity = 'restart-42'
		   AND lifecycle_phase = 'firing' AND outcome = 1`, integrationID).Scan(&deliveryID); err != nil {
		t.Fatalf("202 returned before durable acceptance: %v", err)
	}

	h.plane.program.kill()
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatalf("releasing the processing fence: %v", err)
	}
	if err = h.plane.start(ctx, h.plane.spkiPin); err != nil {
		t.Fatalf("restarting the control plane: %v", err)
	}

	h.await(t, "the accepted webhook delivery to converge after restart", 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			var workComplete, investigations int
			if err := h.truth.pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE work.status = 5),
				       count(DISTINCT investigation.investigation_id)
				  FROM webhook_work AS work
				  LEFT JOIN investigation
				    ON investigation.org_id = work.org_id
				   AND investigation.webhook_work_id = work.work_id
				 WHERE work.org_id = $1 AND work.delivery_id = $2`, organization, deliveryID).
				Scan(&workComplete, &investigations); err != nil {
				return false, err
			}
			return workComplete == 1 && investigations == 1, nil
		})
}

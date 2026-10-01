package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackMessageCapacityDeferralDoesNotConsumeRetries(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	chat := openConversation(t, database, organization, "pending work")
	say(t, database, organization, chat.ID, "hold the pending slot")
	if _, opened, err := database.OpenTurn(ctx, organization, chat.ID, turnWindowLead); err != nil || !opened {
		t.Fatalf("occupying capacity: %v, %v", opened, err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	worker := fixture.worker
	worker.MaxWaitingTurns = 1
	worker.MaxAttempts = 1
	for range 3 {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatalf("deferring accepted message: %v, %v", worked, err)
		}
		delivery, err := database.WebhookDeliveryByID(ctx, organization, fixture.delivery)
		if err != nil || delivery.State != storage.WebhookDeliveryProcessing ||
			delivery.Attempts != 0 || delivery.NextEligibleAt == nil {
			t.Fatalf("capacity deferral consumed retry budget: %+v, %v", delivery, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE webhook_job SET available_at = now() - interval '1 second'
			WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("investigation-worker")); err != nil || !claimed {
		t.Fatalf("releasing the pending slot: %v, %v", claimed, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing after capacity becomes available: %v, %v", worked, err)
	}
	delivery, err := database.WebhookDeliveryByID(ctx, organization, fixture.delivery)
	if err != nil || delivery.State != storage.WebhookDeliverySucceeded || delivery.Attempts != 1 {
		t.Fatalf("deferred message did not recover: %+v, %v", delivery, err)
	}
}

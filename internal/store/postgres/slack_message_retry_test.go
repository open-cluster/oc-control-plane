package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackMessageRetryTerminatesAndReplayPreservesTheQuestion(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_slack_turn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected turn failure'; END $$;
		CREATE TRIGGER reject_slack_turn BEFORE INSERT ON investigation
		FOR EACH ROW EXECUTE FUNCTION reject_slack_turn()`); err != nil {
		t.Fatal(err)
	}
	worker := fixture.worker
	worker.MaxAttempts = 3
	worker.RetryBase = time.Minute
	for attempt := 1; attempt <= 3; attempt++ {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatalf("attempt %d: %v, %v", attempt, worked, err)
		}
		delivery, err := database.WebhookDeliveryByID(ctx, organization, fixture.delivery)
		if err != nil || delivery.Attempts != attempt {
			t.Fatalf("attempt %d delivery: %+v, %v", attempt, delivery, err)
		}
		if attempt < 3 {
			if delivery.State != storage.WebhookDeliveryProcessing || delivery.NextEligibleAt == nil || delivery.LastAttemptAt == nil {
				t.Fatalf("retry was not scheduled: %+v", delivery)
			}
			if delay := delivery.NextEligibleAt.Sub(*delivery.LastAttemptAt); delay != time.Duration(attempt)*time.Minute {
				t.Fatalf("attempt %d retry delay = %s, want %d minutes", attempt, delay, attempt)
			}
			if worked, err := worker.ProcessOne(ctx); err != nil || worked {
				t.Fatalf("retry ignored backoff: %v, %v", worked, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE webhook_job SET available_at = now() - interval '1 second'
				WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
				t.Fatal(err)
			}
		} else if delivery.State != storage.WebhookDeliveryFailed || delivery.FailureClass != "provider-job-failed" {
			t.Fatalf("exhausted retry was not terminal: %+v", delivery)
		}
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("terminal work was reclaimed: %v, %v", worked, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Messages) != 1 || len(detail.Turns) != 0 {
		t.Fatalf("failed turn lost or partially processed the question: %+v, %v", detail, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_slack_turn ON investigation; DROP FUNCTION reject_slack_turn()`); err != nil {
		t.Fatal(err)
	}
	if err := database.ReplayWebhookDelivery(ctx, ownerOf(t, organization), organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing replay: %v, %v", worked, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Messages) != 1 || len(detail.Turns) != 1 {
		t.Fatalf("replay changed the question or duplicated a turn: %+v, %v", detail, err)
	}
	delivery, err := database.WebhookDeliveryByID(ctx, organization, fixture.delivery)
	if err != nil || delivery.State != storage.WebhookDeliverySucceeded || delivery.Attempts != 1 {
		t.Fatalf("replayed delivery: %+v, %v", delivery, err)
	}
}

func TestSlackMessageRecoveryAtPersistedAttemptLimitIsTerminal(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_job
		SET status = 2, attempts = 12, lease_owner = 'interrupted-worker', lease_epoch = 12,
		    lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if worked, err := fixture.worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("recovering exhausted work: %v, %v", worked, err)
	}
	delivery, err := database.WebhookDeliveryByID(ctx, organization, fixture.delivery)
	if err != nil || delivery.State != storage.WebhookDeliveryFailed || delivery.Attempts != 12 {
		t.Fatalf("recovery exceeded the persisted budget or failed to stop: %+v, %v", delivery, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("exhausted work opened a turn or lost the question: %+v, %v", detail, err)
	}
}

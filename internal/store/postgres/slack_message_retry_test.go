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
		work := readSlackMessageWork(t, fixture)
		if work.Attempts != attempt {
			t.Fatalf("attempt %d work: %+v", attempt, work)
		}
		if attempt < 3 {
			if work.Status != storage.SlackMessageRetry {
				t.Fatalf("retry was not scheduled: %+v", work)
			}
			if delay := work.AvailableAt.Sub(work.UpdatedAt); delay != time.Duration(attempt)*time.Minute {
				t.Fatalf("attempt %d retry delay = %s, want %d minutes", attempt, delay, attempt)
			}
			if worked, err := worker.ProcessOne(ctx); err != nil || worked {
				t.Fatalf("retry ignored backoff: %v, %v", worked, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET available_at = now() - interval '1 second'
				WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
				t.Fatal(err)
			}
		} else if work.Status != storage.SlackMessageTerminal || work.FailureClass != "provider-job-failed" {
			t.Fatalf("exhausted retry was not terminal: %+v", work)
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
	if err := database.RecoverSlackMessage(ctx, ownerOf(t, organization), organization,
		fixture.conversation, 1); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing replay: %v, %v", worked, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Messages) != 1 || len(detail.Turns) != 1 {
		t.Fatalf("replay changed the question or duplicated a turn: %+v, %v", detail, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageComplete || work.Attempts != 1 {
		t.Fatalf("recovered work: %+v", work)
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
	if _, err := pool.Exec(ctx, `UPDATE slack_message_work
		SET status = 2, attempts = 12, lease_owner = 'interrupted-worker', lease_epoch = 12,
		    lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if worked, err := fixture.worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("recovering exhausted work: %v, %v", worked, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageTerminal || work.Attempts != 12 {
		t.Fatalf("recovery exceeded the persisted budget or failed to stop: %+v", work)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("exhausted work opened a turn or lost the question: %+v, %v", detail, err)
	}
}

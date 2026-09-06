package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

func TestExpiredSlackClaimCannotAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id, _ := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	reply := claimed(t, database, id, -time.Second)
	err := database.AdvanceSlackReply(ctx, org, id, reply.ClaimToken, slack.Progress{
		Stream: slack.Stream{TS: "stale-message", Native: true}, Sequence: 9,
	})
	if err == nil {
		t.Fatal("an expired worker advanced the reply")
	}
	_, sequence, message, _, found, err := database.SlackReplyState(ctx, org, id)
	if err != nil || !found || sequence != 0 || message != "" {
		t.Fatalf("stale write changed delivery: sequence=%d message=%q found=%v err=%v", sequence, message, found, err)
	}
}

func TestReclaimedSlackReplyRejectsPreviousGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id, _ := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	old := claimed(t, database, id, -time.Second)
	current := claimed(t, database, id, time.Minute)
	if old.ClaimToken == uuid.Nil || current.ClaimToken == uuid.Nil || old.ClaimToken == current.ClaimToken {
		t.Fatal("each claim must have a distinct nonzero token")
	}
	for _, token := range []uuid.UUID{old.ClaimToken, uuid.Nil, uuid.New()} {
		for _, write := range []func() error{
			func() error {
				return database.AdvanceSlackReply(ctx, org, id, token, slack.Progress{Stream: slack.Stream{TS: "stale"}, Sequence: 99})
			},
			func() error { return database.RetrySlackReply(ctx, org, id, token, time.Now(), "stale", true) },
			func() error { return database.CompleteSlackReply(ctx, org, id, token) },
			func() error { return database.ReleaseSlackReply(ctx, org, id, token, time.Now()) },
		} {
			if err := write(); !errors.Is(err, slack.ErrReplyClaimLost) {
				t.Fatalf("stale mutation returned %v", err)
			}
		}
	}
	if err := database.AdvanceSlackReply(ctx, org, id, current.ClaimToken, slack.Progress{Stream: slack.Stream{TS: "current"}, Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteSlackReply(ctx, org, id, current.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := database.RetrySlackReply(ctx, org, id, current.ClaimToken, time.Now(), "late retry", false); !errors.Is(err, slack.ErrReplyClaimLost) {
		t.Fatalf("completed delivery accepted a retry: %v", err)
	}
	_, sequence, message, note, found, err := database.SlackReplyState(ctx, org, id)
	if err != nil || !found || sequence != 2 || message != "current" || note != "" {
		t.Fatalf("delivery changed: sequence=%d message=%q note=%q found=%v err=%v", sequence, message, note, found, err)
	}
}

func TestSlackClaimMigrationPreservesHeldDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id, _ := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	reply := claimed(t, database, id, time.Minute)
	if err := database.AdvanceSlackReply(ctx, org, id, reply.ClaimToken, slack.Progress{Stream: slack.Stream{TS: "retained"}, Sequence: 4}); err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE slack_reply DROP COLUMN lease_owner; DELETE FROM schema_migration WHERE version = '0007_slack_reply_claim'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var owner uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT lease_owner FROM slack_reply WHERE org_id = $1 AND investigation_id = $2`, org.String(), id).Scan(&owner); err != nil || owner == uuid.Nil {
		t.Fatalf("retained ownership: %v %v", owner, err)
	}
	if err = database.CompleteSlackReply(ctx, org, id, owner); err != nil {
		t.Fatal(err)
	}
	_, sequence, message, _, found, err := database.SlackReplyState(ctx, org, id)
	if err != nil || !found || sequence != 4 || message != "retained" {
		t.Fatalf("migration changed delivery: %d %q %v %v", sequence, message, found, err)
	}
}

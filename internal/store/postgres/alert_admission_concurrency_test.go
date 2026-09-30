package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAlertBatchSharesCapacityWithManualConversationAndSlackProducers(t *testing.T) {
	// The fence, four producers, and lock observer each need a connection.
	database := openDatabaseForTest(t, postgresDSN(t)+"&pool_max_conns=6")
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	organization := uuid.MustParse(testOrganization)
	ensureTestOrganization(t, database, organization)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	principal := ownerOf(t, organization)
	chat := openConversation(t, database, organization, "manual conversation")
	slack, err := connectSlack(t, database, organization, "Slack", slackInstallation("TSHARED"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: slack.ID, ContentDigest: randomDigest(t), Channel: "CSHARED", Thread: "1.0",
		Subject: "slack question", ActorID: "USHARED", Text: "investigate",
	}); err != nil {
		t.Fatal(err)
	}
	work, found, err := database.ClaimWebhookJob(ctx, "slack-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("Slack job: found=%t err=%v", found, err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fence.Rollback(ctx) }()
	if _, err := fence.Exec(ctx, `LOCK TABLE investigation IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("a", "a", "A", "2026-09-29T10:00:00Z"),
		alertInvestigationEvent("b", "b", "B", "2026-09-29T10:00:00Z"),
		alertInvestigationEvent("c", "c", "C", "2026-09-29T10:00:00Z"))
	accepted := make(chan error, 1)
	go func() {
		_, err := database.RecordDelivery(ctx, organization, delivery, storage.AlertAdmissionPolicy{MaximumPending: 3})
		accepted <- err
	}()
	awaitBlockedAlertInvestigationInsert(t, ctx, pool)
	manual, web, slackResult := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := database.CreateInvestigation(ctx, principal, organization,
			investigation.NewInvestigation{Subject: "manual", WindowFrom: time.Now(), WindowUntil: time.Now()}, 3)
		manual <- err
	}()
	go func() {
		_, _, _, err := database.AppendMessageAndOpenTurn(ctx, principal, organization, chat.ID,
			conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
				ActorID: principal.UserID().String(), Text: "investigate"}, time.Hour, 3)
		web <- err
	}()
	go func() { slackResult <- database.ApplySlackWebhookJob(ctx, organization, work, time.Hour, 3) }()
	awaitInvestigationAdmissionWaiters(t, ctx, pool, 3)
	if err := fence.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatalf("alert batch: %v", err)
	}
	if err := <-manual; !errors.Is(err, investigation.ErrQueueFull) {
		t.Fatalf("manual producer: %v", err)
	}
	if err := <-web; !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("Conversation producer: %v", err)
	}
	if err := <-slackResult; !errors.Is(err, storage.ErrWebhookJobCapacity) {
		t.Fatalf("Slack producer: %v", err)
	}
	for range 3 {
		if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("alert")); err != nil || !found {
			t.Fatalf("claiming alert batch: found=%t err=%v", found, err)
		}
	}
	if err := database.ApplySlackWebhookJob(ctx, organization, work, time.Hour, 3); err != nil {
		t.Fatalf("Slack retry after alert batch was claimed: %v", err)
	}
}

func awaitBlockedAlertInvestigationInsert(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%INSERT INTO investigation%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

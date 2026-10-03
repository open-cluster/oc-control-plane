package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackWorkContractionPreservesEveryDurableStateAndLease(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if err := storage.MigrateBeforeSlackContractionForTest(ctx, database); err != nil {
		t.Fatal(err)
	}
	organization := uuid.MustParse(testOrganization)
	other := uuid.New()
	ensureTestOrganization(t, database, organization)
	ensureTestOrganization(t, database, other)
	integration, err := connectSlack(t, database, organization, "Slack", slackInstallation("TCONTRACTION"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE webhook_job ALTER COLUMN kind SET DEFAULT 2`); err != nil {
		t.Fatal(err)
	}
	var leased storage.SlackMessageWork
	var terminalDelivery uuid.UUID
	var terminalConversation uuid.UUID
	for status := 1; status <= 5; status++ {
		conversationID, deliveryID, workID := seedLegacySlackMessageWork(
			t, pool, organization, integration.ID, status)
		if status == 2 {
			leased.Organization, leased.IntegrationID, leased.ConversationID = organization, integration.ID, conversationID
			leased.LeaseOwner, leased.LeaseEpoch = "retained-owner", 7
			leased.ID, leased.DeliveryID, leased.MessageSequence = workID, deliveryID, 1
		}
		if status == 4 {
			terminalConversation, terminalDelivery = conversationID, deliveryID
		}
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object(
		'id', job_id, 'row', to_jsonb(work) - 'kind' - 'incident_id' - 'job_id') ORDER BY job_id)::text
		FROM webhook_job work WHERE org_id = $1`, organization).Scan(&before); err != nil {
		t.Fatal(err)
	}
	applied, err := database.Migrate(ctx)
	if err != nil || len(applied) != 3 || applied[0] != "0013_contract_slack_message_work" ||
		applied[1] != "0014_contract_webhook_delivery" || applied[2] != "0015_rename_slack_message_work" {
		t.Fatalf("contraction applied %v: %v", applied, err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object(
		'id', work_id, 'row', to_jsonb(work) - 'work_id') ORDER BY work_id)::text
		FROM slack_message_work work WHERE org_id = $1`, organization).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("contraction changed retained Slack work fields")
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated contraction applied %v: %v", applied, err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, organization, leased, time.Hour); err != nil {
		t.Fatalf("retained owner lost its lease: %v", err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, other, leased, time.Hour); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization renewed retained work: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET available_at = now() - interval '1 second',
		lease_expires_at = now() - interval '1 second' WHERE org_id = $1 AND work_id = $2`, organization, leased.ID); err != nil {
		t.Fatal(err)
	}
	replacement, found, err := database.ClaimSlackMessageWork(ctx, "replacement-owner", time.Minute)
	if err != nil || !found || replacement.ID != leased.ID || replacement.LeaseEpoch != 8 || replacement.Attempts != 3 {
		t.Fatalf("retained lease recovery: %+v, %v, %v", replacement, found, err)
	}
	if err := database.ApplySlackMessageWork(ctx, organization, leased, time.Hour, 0); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("superseded retained lease applied a turn: %v", err)
	}
	if err := database.ApplySlackMessageWork(ctx, organization, replacement, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if err := database.RecoverSlackMessage(ctx, ownerOf(t, other), other, terminalConversation, 1); !errors.Is(err, storage.ErrSlackMessageRecoveryUnavailable) {
		t.Fatalf("another Organization recovered retained work: %v", err)
	}
	if err := database.RecoverSlackMessage(ctx, ownerOf(t, organization), organization, terminalConversation, 1); err != nil {
		t.Fatal(err)
	}
	replayed, found, err := database.ClaimSlackMessageWork(ctx, "replay-owner", time.Minute)
	if err != nil || !found || replayed.DeliveryID != terminalDelivery || replayed.Attempts != 1 || replayed.LeaseEpoch != 8 {
		t.Fatalf("retained terminal work replay: %+v, %v, %v", replayed, found, err)
	}
	if err := database.FailSlackMessageWork(ctx, organization, replayed, false, time.Minute, "retry", "retry diagnostic"); err != nil {
		t.Fatal(err)
	}
	var status storage.SlackMessageWorkStatus
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status, attempts FROM slack_message_work
		WHERE org_id = $1 AND delivery_id = $2`, organization, terminalDelivery).Scan(&status, &attempts); err != nil ||
		status != storage.SlackMessageRetry || attempts != 1 {
		t.Fatalf("retained work cannot retry: status=%s attempts=%d error=%v", status, attempts, err)
	}
}

func seedLegacySlackMessageWork(
	t *testing.T, pool *pgxpool.Pool, organization, integration uuid.UUID, status int,
) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	conversationID, deliveryID, workID := uuid.New(), uuid.New(), uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO webhook_delivery
			(delivery_id, org_id, integration_id, content_digest, provider_identity, request_id)
		VALUES ($1, $2, $3, $4, $1::text, '');
		INSERT INTO conversation (conversation_id, org_id, surface, subject)
		VALUES ($5, $2, 2, 'retained question');
		INSERT INTO conversation_message
			(conversation_id, org_id, sequence, role, actor_kind, actor_id, text, window_from, window_until)
		VALUES ($5, $2, 1, 1, 2, 'UCONTRACTION', 'investigate', now() - interval '1 hour', now());
		INSERT INTO webhook_job
			(job_id, org_id, kind, status, delivery_id, integration_id, conversation_id, message_sequence,
			 attempts, available_at, lease_owner, lease_epoch, lease_expires_at,
			 failure_class, failure_message, created_at, updated_at)
		VALUES ($6, $2, 2, $7, $1, $3, $5, 1, $7, now() + interval '1 day',
			CASE WHEN $7 = 2 THEN 'retained-owner' ELSE '' END, 7,
			CASE WHEN $7 = 2 THEN now() + interval '1 hour' ELSE NULL END,
			CASE WHEN $7 IN (3,4) THEN 'retained-failure' ELSE '' END,
			CASE WHEN $7 IN (3,4) THEN 'retained diagnostic' ELSE '' END,
			'2026-09-01T12:00:00Z', '2026-09-02T12:00:00Z')`,
		deliveryID, organization, integration, randomDigest(t), conversationID, workID, status)
	if err != nil {
		t.Fatal(err)
	}
	return conversationID, deliveryID, workID
}

func TestSlackWorkContractionRefusesUnrepairedAlertWork(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if err := storage.MigrateBeforeSlackContractionForTest(ctx, database); err != nil {
		t.Fatal(err)
	}
	organization := uuid.MustParse(testOrganization)
	ensureTestOrganization(t, database, organization)
	integration := alertmanagerIntegration(t, database, organization)
	incident := recordIncident(t, database, organization, integration, "unrepaired")
	job := seedLegacyAlertJob(t, database, organization, integration, incident, 1, time.Now().UTC())
	if _, err := database.Migrate(ctx); err == nil {
		t.Fatal("contraction silently discarded unrepaired alert work")
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_job WHERE org_id = $1 AND job_id = $2 AND kind = 1)
		AND NOT EXISTS (SELECT 1 FROM schema_migration WHERE version = '0013_contract_slack_message_work')`,
		organization, job).Scan(&retained); err != nil || !retained {
		t.Fatalf("refused contraction changed retained state: %v", err)
	}
}

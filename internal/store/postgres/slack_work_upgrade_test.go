package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
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
	for status := 1; status <= 5; status++ {
		message, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
			Integration: integration.ID, ContentDigest: randomDigest(t), Channel: "CCONTRACTION",
			Thread: fmt.Sprintf("%d.0", status), Subject: "retained question", ActorID: "UCONTRACTION", Text: "investigate",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE webhook_job SET status = $3::smallint, attempts = $3::smallint,
			available_at = now() + interval '1 day', lease_epoch = 7,
			lease_owner = CASE WHEN $3::smallint = 2 THEN 'retained-owner' ELSE '' END,
			lease_expires_at = CASE WHEN $3::smallint = 2 THEN now() + interval '1 hour' ELSE NULL END,
			failure_class = CASE WHEN $3::smallint IN (3,4) THEN 'retained-failure' ELSE '' END,
			failure_message = CASE WHEN $3::smallint IN (3,4) THEN 'retained diagnostic' ELSE '' END,
			created_at = '2026-09-01T12:00:00Z', updated_at = '2026-09-02T12:00:00Z'
			WHERE org_id = $1 AND conversation_id = $2`, organization, message.Conversation, status); err != nil {
			t.Fatal(err)
		}
		if status == 2 {
			leased.Organization, leased.IntegrationID, leased.ConversationID = organization, integration.ID, message.Conversation
			leased.LeaseOwner, leased.LeaseEpoch = "retained-owner", 7
			if err := pool.QueryRow(ctx, `SELECT job_id, delivery_id, message_sequence FROM webhook_job
				WHERE org_id = $1 AND conversation_id = $2`, organization, message.Conversation).
				Scan(&leased.ID, &leased.DeliveryID, &leased.MessageSequence); err != nil {
				t.Fatal(err)
			}
		}
		if status == 4 {
			if err := pool.QueryRow(ctx, `SELECT delivery_id FROM webhook_job
				WHERE org_id = $1 AND conversation_id = $2`, organization, message.Conversation).Scan(&terminalDelivery); err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot := func() string {
		t.Helper()
		var rows string
		if err := pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(work) - 'kind' - 'incident_id' ORDER BY job_id)::text
			FROM webhook_job work WHERE org_id = $1`, organization).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := snapshot()
	applied, err := database.Migrate(ctx)
	if err != nil || len(applied) != 1 || applied[0] != "0013_contract_slack_message_work" {
		t.Fatalf("contraction applied %v: %v", applied, err)
	}
	if after := snapshot(); after != before {
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
	if _, err := pool.Exec(ctx, `UPDATE webhook_job SET available_at = now() - interval '1 second',
		lease_expires_at = now() - interval '1 second' WHERE org_id = $1 AND job_id = $2`, organization, leased.ID); err != nil {
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
	if err := database.ReplayWebhookDelivery(ctx, ownerOf(t, other), other, terminalDelivery); !errors.Is(err, storage.ErrWebhookDeliveryUnknown) {
		t.Fatalf("another Organization replayed retained work: %v", err)
	}
	if err := database.ReplayWebhookDelivery(ctx, ownerOf(t, organization), organization, terminalDelivery); err != nil {
		t.Fatal(err)
	}
	replayed, found, err := database.ClaimSlackMessageWork(ctx, "replay-owner", time.Minute)
	if err != nil || !found || replayed.DeliveryID != terminalDelivery || replayed.Attempts != 1 || replayed.LeaseEpoch != 8 {
		t.Fatalf("retained terminal work replay: %+v, %v, %v", replayed, found, err)
	}
	if err := database.FailSlackMessageWork(ctx, organization, replayed, false, time.Minute, "retry", "retry diagnostic"); err != nil {
		t.Fatal(err)
	}
	delivery, err := database.WebhookDeliveryByID(ctx, organization, terminalDelivery)
	if err != nil || delivery.State != "processing" || delivery.Attempts != 1 || delivery.NextEligibleAt == nil {
		t.Fatalf("retained work cannot retry: %+v, %v", delivery, err)
	}
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

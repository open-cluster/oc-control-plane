package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestLegacyAlertMigrationRepairsEveryStateAndPreservesManualAndSlackWork(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if err := storage.MigrateBeforeAlertBackfillForTest(ctx, database); err != nil {
		t.Fatal(err)
	}
	organization := uuid.MustParse(testOrganization)
	ensureTestOrganization(t, database, organization)
	integration := alertmanagerIntegration(t, database, organization)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	incidents := make([]uuid.UUID, 5)
	age := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var linked uuid.UUID
	for index := range incidents {
		incidents[index] = recordIncident(t, database, organization, integration, fmt.Sprintf("legacy-%d", index))
		if _, err := pool.Exec(ctx, `UPDATE incident SET first_seen_at = '2026-08-31T11:00:00Z',
			last_seen_at = '2026-08-31T12:00:00Z' WHERE org_id = $1 AND incident_id = $2`,
			organization, incidents[index]); err != nil {
			t.Fatal(err)
		}
		job := seedLegacyAlertJob(t, database, organization, integration, incidents[index], index+1, age)
		if index == 4 {
			linked = uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO investigation
				(investigation_id, org_id, incident_id, subject, window_from, window_until, webhook_job_id)
				VALUES ($1, $2, $3, 'existing alert', now(), now(), $4)`, linked, organization, incidents[index], job); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedLegacyAlertJob(t, database, organization, integration, incidents[0], 3, age.Add(time.Hour))
	manual, err := database.CreateInvestigation(ctx, ownerOf(t, organization), organization,
		investigation.NewInvestigation{IncidentID: incidents[0]}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordDelivery(ctx, organization,
		alertInvestigationDelivery(integration, alertInvestigationEvent("native", "native", "Native automatic",
			"2026-08-31T11:00:00Z")), storage.AlertAdmissionPolicy{WindowLead: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	var nativeIncident, nativeAutomatic uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT incident_id, investigation_id FROM investigation
		WHERE org_id = $1 AND subject = 'Native automatic'`, organization).Scan(&nativeIncident, &nativeAutomatic); err != nil {
		t.Fatal(err)
	}
	incidents = append(incidents, nativeIncident)
	seedLegacyAlertJob(t, database, organization, integration, nativeIncident, 4, age)
	facts := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'deliveries', (SELECT jsonb_agg(to_jsonb(d) ORDER BY delivery_id) FROM webhook_delivery AS d WHERE org_id = $1),
			'alerts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY alert_event_id) FROM alert_event AS a WHERE org_id = $1),
			'incidents', (SELECT jsonb_agg(to_jsonb(i) ORDER BY incident_id) FROM incident AS i WHERE org_id = $1))::text`,
			organization).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	slack, err := connectSlack(t, database, organization, "Slack", slackInstallation("TBACKFILL"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE webhook_job ALTER COLUMN kind SET DEFAULT 2`); err != nil {
		t.Fatal(err)
	}
	slackOutcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: slack.ID, ContentDigest: randomDigest(t), Channel: "CBACKFILL", Thread: "1.0",
		Subject: "Slack question", ActorID: "UBACKFILL", Text: "investigate",
	})
	if err != nil {
		t.Fatal(err)
	}
	var slackBefore string
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(job) - 'kind' - 'incident_id')::text FROM webhook_job AS job
		WHERE org_id = $1 AND conversation_id = $2`, organization, slackOutcome.Conversation).Scan(&slackBefore); err != nil {
		t.Fatal(err)
	}
	factsBefore := facts()
	applied, err := database.Migrate(ctx)
	if err != nil || len(applied) != 2 || applied[0] != "0012_retire_alert_webhook_jobs" ||
		applied[1] != "0013_contract_slack_message_work" {
		t.Fatalf("alert backfill applied %v: %v", applied, err)
	}
	for index, incident := range incidents {
		var automatic int
		var created time.Time
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT count(*), min(created_at), min(investigation_id::text)::uuid
			FROM investigation WHERE org_id = $1 AND incident_id = $2 AND automatic_incident`,
			organization, incident).Scan(&automatic, &created, &id); err != nil || automatic != 1 {
			t.Fatalf("legacy status %d: automatic=%d err=%v", index+1, automatic, err)
		}
		if index < 4 && !created.Equal(age) {
			t.Fatalf("legacy status %d lost queue age: %s", index+1, created)
		}
		if index == 4 && id != linked {
			t.Fatal("the linked Investigation was replaced")
		}
		if index == 5 && id != nativeAutomatic {
			t.Fatal("the existing automatic Investigation was replaced")
		}
		if index < 4 {
			read, err := database.Investigation(ctx, organization, id)
			if err != nil || read.Subject != "a failure" ||
				read.WindowFrom.UTC().Format(time.RFC3339) != "2026-08-31T09:00:00Z" ||
				read.WindowUntil.UTC().Format(time.RFC3339) != "2026-08-31T12:00:00Z" {
				t.Fatalf("repaired Investigation uses incorrect Incident context: %+v: %v", read, err)
			}
		}
	}
	if _, err := database.Investigation(ctx, organization, manual.ID); err != nil {
		t.Fatalf("manual Investigation lost: %v", err)
	}
	var slackAfter string
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(job) - 'kind' - 'incident_id')::text FROM webhook_job AS job
		WHERE org_id = $1 AND conversation_id = $2`, organization, slackOutcome.Conversation).Scan(&slackAfter); err != nil || slackAfter != slackBefore {
		t.Fatalf("Slack work changed: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_job WHERE org_id = $1`, organization).
		Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("expected only retained Slack work: %d: %v", remaining, err)
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated migration applied %v: %v", applied, err)
	}
	if factsAfter := facts(); factsAfter != factsBefore {
		t.Fatal("migration changed accepted deliveries, Alert Events, or Incidents")
	}
	if _, claimed, found, err := database.ClaimInvestigation(ctx, aClaim("backfill-runner")); err != nil || !found || !claimed.CreatedAt.Equal(age) {
		t.Fatalf("repaired work is not claimable with its original queue age: found=%t err=%v", found, err)
	}
	work, found, err := database.ClaimSlackMessageWork(ctx, "slack-worker", time.Minute)
	if err != nil || !found || work.ConversationID != slackOutcome.Conversation || work.IntegrationID != slack.ID {
		t.Fatalf("remaining Slack work is not claimable: found=%t err=%v", found, err)
	}
	if err := database.ApplySlackMessageWork(ctx, organization, work, 2*time.Hour, 0); err != nil {
		t.Fatalf("remaining Slack work no longer opens its turn: %v", err)
	}
}

func seedLegacyAlertJob(t *testing.T, database *storage.Database, organization, integration, incident uuid.UUID,
	status int, created time.Time,
) uuid.UUID {
	t.Helper()
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	delivery, job := uuid.New(), uuid.New()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_delivery
		(delivery_id, org_id, integration_id, content_digest, provider_identity)
		VALUES ($1, $2, $3, $4, $5)`, delivery, organization, integration, randomDigest(t), delivery.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_job
		(job_id, org_id, kind, status, delivery_id, integration_id, incident_id,
		 lease_owner, lease_expires_at, failure_class, created_at, updated_at)
		VALUES ($1, $2, 1, $3::smallint, $4, $5, $6,
		 CASE WHEN $3::smallint = 2 THEN 'old-worker' ELSE '' END,
		 CASE WHEN $3::smallint = 2 THEN now() + interval '1 hour' ELSE NULL END,
		 CASE WHEN $3::smallint IN (3,4) THEN 'legacy-failure' ELSE '' END, $7, $7)`,
		job, organization, status, delivery, integration, incident, created); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestLegacyAlertMigrationRejectsAmbiguousInvestigationsWithoutDeletingWork(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if err := storage.MigrateBeforeAlertBackfillForTest(ctx, database); err != nil {
		t.Fatal(err)
	}
	organization := uuid.MustParse(testOrganization)
	ensureTestOrganization(t, database, organization)
	integration := alertmanagerIntegration(t, database, organization)
	incident := recordIncident(t, database, organization, integration, "ambiguous")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		job := seedLegacyAlertJob(t, database, organization, integration, incident, 5, time.Now().UTC())
		if _, err := pool.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, incident_id, subject, window_from, window_until, webhook_job_id)
			VALUES ($1, $2, $3, 'legacy alert', now(), now(), $4)`, uuid.New(), organization, incident, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Migrate(ctx); err == nil {
		t.Fatal("ambiguous legacy Investigations were silently accepted")
	}
	var jobs, investigations, automatic int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM webhook_job WHERE org_id = $1 AND kind = 1),
		count(*), count(*) FILTER (WHERE automatic_incident)
		FROM investigation WHERE org_id = $1 AND incident_id = $2`, organization, incident).
		Scan(&jobs, &investigations, &automatic); err != nil || jobs != 2 || investigations != 2 || automatic != 0 {
		t.Fatalf("ambiguous repair was not rolled back: jobs=%d investigations=%d automatic=%d err=%v",
			jobs, investigations, automatic, err)
	}
}

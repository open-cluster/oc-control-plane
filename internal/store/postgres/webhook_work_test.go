package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestWebhookWorkOpensExactlyOneInvestigationAcrossEffectReplay(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	outcome, err := database.RecordDelivery(context.Background(), organization, storage.Delivery{
		Integration: integration, BodyDigest: make([]byte, 32), AlertEvents: []storage.AlertEvent{{
			SourceKey: "alert-1", GroupingKey: "service-api", Status: storage.AlertEventFiring,
			Title: "API unavailable", StartedAt: started,
		}},
	})
	if err != nil || outcome.IncidentsOpened != 1 {
		t.Fatalf("recording firing delivery: %+v %v", outcome, err)
	}
	work, found, err := database.ClaimWebhookWork(context.Background(), "worker-a", time.Minute)
	if err != nil || !found {
		t.Fatalf("claiming work: %v found=%v", err, found)
	}

	// Simulate a prior worker that committed the stable effect but died before acknowledging
	// it. The retry must find the origin and complete without creating another Investigation.
	pool, _ := database.Pool(organization)
	seeded := uuid.New()
	if _, err = pool.Exec(context.Background(), `
		INSERT INTO investigation (investigation_id, org_id, incident_id, integration_id,
		                           subject, window_from, window_until, created_by, webhook_work_id)
		VALUES ($1,$2,$3,$4,'API unavailable',$5,$6,'webhook',$7)`, seeded,
		organization.String(), work.IncidentID, integration, started.Add(-time.Hour), started, work.ID); err != nil {
		t.Fatalf("seeding prior effect: %v", err)
	}
	got, err := database.ApplyAlertWebhookWork(context.Background(), organization, work, time.Hour, 0)
	if err != nil {
		t.Fatalf("replaying work: %v", err)
	}
	if got != seeded {
		t.Fatalf("replay returned %s, want existing %s", got, seeded)
	}
	var count int
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM investigation WHERE org_id=$1 AND webhook_work_id=$2`, organization.String(), work.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("investigation count=%d err=%v", count, err)
	}
}

func TestWebhookWorkLeaseEpochRejectsAStaleWorker(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	_, err := database.RecordDelivery(context.Background(), organization, storage.Delivery{
		Integration: integration, BodyDigest: append(make([]byte, 31), 1), AlertEvents: []storage.AlertEvent{{
			SourceKey: "alert-lease", Status: storage.AlertEventFiring, Title: "lease",
			StartedAt: time.Now().UTC().Add(-time.Minute),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, found, err := database.ClaimWebhookWork(context.Background(), "old", time.Millisecond)
	if err != nil || !found {
		t.Fatalf("first claim: %v found=%v", err, found)
	}
	time.Sleep(5 * time.Millisecond)
	current, found, err := database.ClaimWebhookWork(context.Background(), "new", time.Minute)
	if err != nil || !found {
		t.Fatalf("second claim: %v found=%v", err, found)
	}
	if current.LeaseEpoch <= stale.LeaseEpoch {
		t.Fatalf("epoch did not advance: old=%d new=%d", stale.LeaseEpoch, current.LeaseEpoch)
	}
	if err = database.CompleteWebhookWork(context.Background(), organization, stale); !errors.Is(err, storage.ErrWebhookWorkLeaseLost) {
		t.Fatalf("stale completion=%v, want lease lost", err)
	}
}

func TestTerminalWebhookReplayRestoresTheEntireAttemptBudget(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	_, err := database.RecordDelivery(context.Background(), organization, storage.Delivery{
		Integration: integration, BodyDigest: append(make([]byte, 31), 2),
		AlertEvents: []storage.AlertEvent{{SourceKey: "replay-budget", Status: storage.AlertEventFiring,
			Title: "retry budget", StartedAt: time.Now().UTC().Add(-time.Minute)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := ownerOf(t, organization)
	for round := 0; round < 15; round++ {
		work, found, claimErr := database.ClaimWebhookWork(context.Background(), "worker", time.Minute)
		if claimErr != nil || !found {
			t.Fatalf("round %d claim: found=%v err=%v", round, found, claimErr)
		}
		if work.Attempts != 1 {
			t.Fatalf("round %d attempts=%d, want a restored budget", round, work.Attempts)
		}
		if err = database.FailWebhookWork(context.Background(), organization, work, true,
			0, "provider-work-failed", "safe failure"); err != nil {
			t.Fatalf("round %d terminal failure: %v", round, err)
		}
		if err = database.ReplayWebhookWork(context.Background(), principal, organization, work.ID); err != nil {
			t.Fatalf("round %d replay: %v", round, err)
		}
	}
}

func TestTerminalWebhookReplayRollsBackWhenItsAuditCannotBeWritten(t *testing.T) {
	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	if _, err := database.RecordDelivery(context.Background(), organization, storage.Delivery{
		Integration: integration, BodyDigest: append(make([]byte, 31), 3),
		AlertEvents: []storage.AlertEvent{{SourceKey: "audit-replay", Status: storage.AlertEventFiring,
			Title: "replay audit", StartedAt: time.Now().UTC().Add(-time.Minute)}},
	}); err != nil {
		t.Fatal(err)
	}
	work, found, err := database.ClaimWebhookWork(context.Background(), "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claiming work: found=%t error=%v", found, err)
	}
	if err = database.FailWebhookWork(context.Background(), organization, work, true,
		0, "provider-work-failed", "safe failure"); err != nil {
		t.Fatalf("recording terminal work: %v", err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `DROP TABLE audit_event`); err != nil {
		t.Fatal(err)
	}
	if err = database.ReplayWebhookWork(context.Background(), ownerOf(t, organization),
		organization, work.ID); !errors.Is(err, audit.ErrWriteFailed) {
		t.Fatalf("replay without its audit = %v, want audit write failure", err)
	}
	terminal, err := database.TerminalWebhookWorkByID(context.Background(), organization, work.ID)
	if err != nil || terminal.Status != storage.WebhookWorkTerminal || terminal.Attempts != 1 {
		t.Fatalf("unaudited replay changed terminal work: %+v error=%v", terminal, err)
	}
}

func TestWebhookClaimsSkipAnotherOrganizationsLockedExhaustedWork(t *testing.T) {
	t.Parallel()
	database, first := migratedDatabase(t)
	second, err := tenancy.NewOrganization("another-webhook-organization")
	if err != nil {
		t.Fatal(err)
	}
	for index, organization := range []tenancy.Organization{first, second} {
		integration := alertmanagerIntegration(t, database, organization)
		if _, err = database.RecordDelivery(context.Background(), organization, storage.Delivery{
			Integration: integration, BodyDigest: append(make([]byte, 31), byte(index+10)),
			AlertEvents: []storage.AlertEvent{{SourceKey: "locked-exhausted", Status: storage.AlertEventFiring,
				Title: "worker isolation", StartedAt: time.Now().UTC().Add(-time.Minute)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := database.Pool(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `
		UPDATE webhook_work SET attempts = 12 WHERE org_id = $1`, first.String()); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err = lock.Exec(context.Background(), `
		SELECT work_id FROM webhook_work WHERE org_id = $1 FOR UPDATE`, first.String()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	work, found, err := database.ClaimWebhookWork(ctx, "independent-worker", time.Minute)
	if err != nil || !found || work.Organization != second {
		t.Fatalf("another organization's locked exhausted row blocked ready work: found=%t org=%s err=%v",
			found, work.Organization.String(), err)
	}
}

func TestAlertWebhookWorkRespectsTheSharedOrganizationInvestigationLimit(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	for index := 0; index < 2; index++ {
		if _, err := database.RecordDelivery(context.Background(), organization, storage.Delivery{
			Integration: integration, BodyDigest: append(make([]byte, 31), byte(index+20)),
			AlertEvents: []storage.AlertEvent{{
				SourceKey: uuid.NewString(), GroupingKey: uuid.NewString(),
				Status: storage.AlertEventFiring, Title: "shared capacity",
				StartedAt: time.Now().UTC().Add(-time.Minute),
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, found, err := database.ClaimWebhookWork(context.Background(), "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claiming first work: found=%t error=%v", found, err)
	}
	if _, err = database.ApplyAlertWebhookWork(context.Background(), organization,
		first, time.Hour, 1); err != nil {
		t.Fatalf("opening the first Investigation: %v", err)
	}
	second, found, err := database.ClaimWebhookWork(context.Background(), "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claiming second work: found=%t error=%v", found, err)
	}
	if _, err = database.ApplyAlertWebhookWork(context.Background(), organization,
		second, time.Hour, 1); !errors.Is(err, storage.ErrWebhookWorkCapacity) {
		t.Fatalf("a second waiting Investigation bypassed the Organization limit: %v", err)
	}
}

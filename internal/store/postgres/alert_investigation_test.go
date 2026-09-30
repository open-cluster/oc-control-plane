package storage_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAcceptedAlertDeliveryLeavesAnInvestigationClaimableWithoutAWebhookWorker(t *testing.T) {
	database, organization := migratedDatabase(t)
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("b", "group", "Earlier source", "2026-09-29T09:00:00Z"),
		alertInvestigationEvent("a", "group", "Checkout unavailable", "2026-09-29T10:00:00Z"))
	ctx := context.Background()
	outcome, err := database.RecordDelivery(ctx, organization, delivery,
		storage.AlertAdmissionPolicy{WindowLead: time.Hour})
	if err != nil || outcome.IncidentsOpened != 1 {
		t.Fatalf("accepting alert: %+v, %v", outcome, err)
	}
	org, claimed, found, err := database.ClaimInvestigation(ctx, aClaim("alert-runner"))
	if err != nil || !found || org != organization {
		t.Fatalf("claim after acceptance: found=%t org=%s err=%v", found, org, err)
	}
	read, err := database.Investigation(ctx, organization, claimed.ID)
	if err != nil || read.IncidentID == uuid.Nil || read.Subject != "Checkout unavailable" {
		t.Fatalf("accepted Investigation: %+v, %v", read, err)
	}
	if read.WindowFrom.UTC().Format(time.RFC3339) != "2026-09-29T08:00:00Z" ||
		read.WindowUntil.UTC().Format(time.RFC3339) != "2026-09-29T10:00:00Z" {
		t.Fatalf("Investigation used incomplete Incident window: %s to %s", read.WindowFrom, read.WindowUntil)
	}
	if _, found, err := database.ClaimWebhookJob(ctx, "unused-alert-worker", time.Minute); err != nil || found {
		t.Fatalf("new alert delivery queued webhook work: found=%t err=%v", found, err)
	}
	page, err := database.WebhookDeliveries(ctx, organization, "", storage.Page{Limit: 10})
	if err != nil || len(page.Deliveries) != 1 || page.Deliveries[0].State != storage.WebhookDeliverySucceeded ||
		page.Deliveries[0].Attempts != 0 {
		t.Fatalf("accepted alert delivery projection: %+v, %v", page, err)
	}
	if err := database.ReplayWebhookDelivery(ctx, ownerOf(t, organization), organization,
		page.Deliveries[0].ID); !errors.Is(err, storage.ErrWebhookDeliveryUnknown) {
		t.Fatalf("succeeded alert delivery replay=%v, want no-op conflict", err)
	}
}

func TestAlertBatchRefusalRollsBackAndRetrySucceedsAfterCapacityIsClaimed(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	policy := storage.AlertAdmissionPolicy{WindowLead: time.Hour, MaximumPending: 2}
	existing := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("existing", "existing", "Existing failure", "2026-09-29T09:00:00Z"))
	if _, err := database.RecordDelivery(ctx, organization, existing, policy); err != nil {
		t.Fatal(err)
	}
	resolution := existing.AlertEvents[0]
	resolution.Status = alertevent.AlertEventResolved
	resolution.ResolvedAt = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	delivery := alertInvestigationDelivery(existing.Integration, resolution,
		alertInvestigationEvent("one", "one", "First failure", "2026-09-29T10:00:00Z"),
		alertInvestigationEvent("two", "two", "Second failure", "2026-09-29T10:00:00Z"))
	var full storage.AlertCapacityError
	if _, err := database.RecordDelivery(ctx, organization, delivery, policy); !errors.As(err, &full) {
		t.Fatalf("batch with only one of its two slots available: %v", err)
	}
	page, err := database.QueryIncidents(ctx, organization, incident.Query{Sort: "lastSeenAt", Limit: 50})
	if err != nil || len(page.Incidents) != 1 || page.Incidents[0].Status != incident.StatusOpen {
		t.Fatalf("refused delivery left Incidents: %+v, %v", page, err)
	}
	if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("existing-runner")); err != nil || !found {
		t.Fatalf("claiming existing capacity: found=%t err=%v", found, err)
	}
	accepted, err := database.RecordDelivery(ctx, organization, delivery, policy)
	if err != nil || accepted.Duplicate || accepted.IncidentsOpened != 2 {
		t.Fatalf("retry after capacity released: %+v, %v", accepted, err)
	}
}

func TestOversizedAlertBatchIsPermanentAndLeavesNoDeliveryFacts(t *testing.T) {
	database, organization := migratedDatabase(t)
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("one", "one", "First failure", "2026-09-29T10:00:00Z"),
		alertInvestigationEvent("two", "two", "Second failure", "2026-09-29T10:00:00Z"))
	ctx := context.Background()
	_, err := database.RecordDelivery(ctx, organization, delivery,
		storage.AlertAdmissionPolicy{MaximumPending: 1})
	var permanent storage.AlertBatchTooLargeError
	if !errors.As(err, &permanent) {
		t.Fatalf("oversized batch error=%v, want permanent refusal", err)
	}
	assertNoAlertDeliveryFacts(t, database, organization)
}

func TestLateAutomaticInvestigationFailureRollsBackTheCompleteDelivery(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE investigation ADD CONSTRAINT test_late_alert_failure
		CHECK (subject <> 'late failure')`); err != nil {
		t.Fatal(err)
	}
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("a", "one", "Good subject", "2026-09-29T10:00:00Z"),
		alertInvestigationEvent("b", "two", "late failure", "2026-09-29T10:00:00Z"))
	if _, err := database.RecordDelivery(ctx, organization, delivery, storage.AlertAdmissionPolicy{}); err == nil {
		t.Fatal("accepted delivery despite late Investigation constraint violation")
	}
	assertNoAlertDeliveryFacts(t, database, organization)
	if _, err := pool.Exec(ctx, `ALTER TABLE investigation DROP CONSTRAINT test_late_alert_failure`); err != nil {
		t.Fatal(err)
	}
	outcome, err := database.RecordDelivery(ctx, organization, delivery, storage.AlertAdmissionPolicy{})
	if err != nil || outcome.Duplicate || outcome.IncidentsOpened != 2 {
		t.Fatalf("retry after late failure: %+v, %v", outcome, err)
	}
}

func TestAlertDuplicateAndIncidentUpdatesSkipTheFullCapacityLock(t *testing.T) {
	database, organization := migratedDatabase(t)
	policy := storage.AlertAdmissionPolicy{MaximumPending: 1}
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("a", "group", "Failure", "2026-09-29T10:00:00Z"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := database.RecordDelivery(ctx, organization, delivery, policy); err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if err := storage.ReserveWaitingInvestigationsForTest(ctx, lock, organization, 1, 0); err != nil {
		t.Fatal(err)
	}
	if outcome, err := database.RecordDelivery(ctx, organization, delivery, policy); err != nil || !outcome.Duplicate {
		t.Fatalf("duplicate while Organization lock is held: %+v, %v", outcome, err)
	}
	joined := alertInvestigationDelivery(delivery.Integration,
		alertInvestigationEvent("b", "group", "Joined failure", "2026-09-29T10:05:00Z"))
	if outcome, err := database.RecordDelivery(ctx, organization, joined, policy); err != nil || outcome.IncidentsJoined != 1 {
		t.Fatalf("joined alert at full capacity: %+v, %v", outcome, err)
	}
	resolved := append([]alertevent.AlertEvent(nil), delivery.AlertEvents...)
	resolved = append(resolved, joined.AlertEvents...)
	for index := range resolved {
		resolved[index].Status = alertevent.AlertEventResolved
		resolved[index].ResolvedAt = time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	}
	if outcome, err := database.RecordDelivery(ctx, organization,
		alertInvestigationDelivery(delivery.Integration, resolved...), policy); err != nil || outcome.IncidentsOpened != 0 {
		t.Fatalf("resolution at full capacity: %+v, %v", outcome, err)
	}
	if _, err := database.RecordDelivery(ctx, organization,
		alertInvestigationDelivery(delivery.Integration, delivery.AlertEvents...), policy); err != nil {
		t.Fatalf("late firing at full capacity: %v", err)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_, first, found, err := database.ClaimInvestigation(ctx, aClaim("first"))
	if err != nil || !found {
		t.Fatalf("first automatic Investigation: %t %v", found, err)
	}
	if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("unexpected")); err != nil || found {
		t.Fatalf("duplicate automatic Investigation after updates: %t %v", found, err)
	}
	occurrence := alertInvestigationDelivery(delivery.Integration,
		alertInvestigationEvent("a", "group", "New occurrence", "2026-09-29T12:00:00Z"))
	if outcome, err := database.RecordDelivery(ctx, organization, occurrence, policy); err != nil || outcome.IncidentsOpened != 1 {
		t.Fatalf("new occurrence after resolution: %+v %v", outcome, err)
	}
	_, second, found, err := database.ClaimInvestigation(ctx, aClaim("second"))
	if err != nil || !found || first.IncidentID == second.IncidentID {
		t.Fatalf("new occurrence Investigation: %+v found=%t err=%v", second, found, err)
	}
	if _, err := database.CreateInvestigation(ctx, ownerOf(t, organization), organization,
		investigation.NewInvestigation{IncidentID: first.IncidentID, Subject: "manual follow-up",
			WindowFrom: first.WindowFrom, WindowUntil: first.WindowUntil}, 1); err != nil {
		t.Fatalf("manual Investigation alongside automatic: %v", err)
	}
}

func TestConcurrentExactAlertDuplicatesOpenOneAutomaticInvestigation(t *testing.T) {
	database, organization := migratedDatabase(t)
	delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
		alertInvestigationEvent("one", "one", "Failure", "2026-09-29T10:00:00Z"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type answer struct {
		outcome storage.DeliveryOutcome
		err     error
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			outcome, err := database.RecordDelivery(ctx, organization, delivery,
				storage.AlertAdmissionPolicy{MaximumPending: 1})
			answers <- answer{outcome, err}
		}()
	}
	close(start)
	var opened, duplicates int
	for range 2 {
		got := <-answers
		if got.err != nil {
			t.Fatal(got.err)
		}
		opened += got.outcome.IncidentsOpened
		if got.outcome.Duplicate {
			duplicates++
		}
	}
	if opened != 1 || duplicates != 1 {
		t.Fatalf("concurrent duplicates opened=%d duplicate=%d", opened, duplicates)
	}
	if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("one")); err != nil || !found {
		t.Fatalf("automatic Investigation claim: %t %v", found, err)
	}
	if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("two")); err != nil || found {
		t.Fatalf("duplicate Investigation claim: %t %v", found, err)
	}
}

func assertNoAlertDeliveryFacts(t *testing.T, database *storage.Database, organization uuid.UUID) {
	t.Helper()
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	var deliveries, alerts, incidents, investigations int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM webhook_delivery WHERE org_id = $1),
		(SELECT count(*) FROM alert_event WHERE org_id = $1),
		(SELECT count(*) FROM incident WHERE org_id = $1),
		(SELECT count(*) FROM investigation WHERE org_id = $1)`, organization).
		Scan(&deliveries, &alerts, &incidents, &investigations); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || alerts != 0 || incidents != 0 || investigations != 0 {
		t.Fatalf("rolled-back facts: deliveries=%d alerts=%d Incidents=%d Investigations=%d",
			deliveries, alerts, incidents, investigations)
	}
}

func alertInvestigationDelivery(integration uuid.UUID, events ...alertevent.AlertEvent) storage.Delivery {
	digest := sha256.Sum256([]byte(uuid.NewString()))
	return storage.Delivery{Integration: integration, AlertDelivery: alertevent.AlertDelivery{
		ContentDigest: digest[:], AlertEvents: events,
	}}
}

func alertInvestigationEvent(source, group, title, began string) alertevent.AlertEvent {
	started, err := time.Parse(time.RFC3339, began)
	if err != nil {
		panic(err)
	}
	return alertevent.AlertEvent{SourceKey: source, GroupingKey: group,
		Title: title, Status: alertevent.AlertEventFiring, StartedAt: started}
}

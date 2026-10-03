package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func recordIncident(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	integration uuid.UUID, key string,
) uuid.UUID {
	t.Helper()

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	if _, err = pool.Exec(context.Background(), `
		INSERT INTO incident
			(incident_id, org_id, integration_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, 'a failure', 1, $5, $5, now())`,
		id, organization, integration, key, now); err != nil {
		t.Fatalf("recording an incident incident: %v", err)
	}
	return id
}

func TestAMerge_LeavesBothRecordsIntact(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)

	absorbed := recordIncident(t, database, organization, integration, "group-a")
	surviving := recordIncident(t, database, organization, integration, "group-b")

	after, err := database.MergeIncidents(
		context.Background(), ownerOf(t, organization), organization, incident.Merge{
			Absorbed: absorbed, Into: surviving, Reason: "one rollout, two alerts",
		})
	if err != nil {
		t.Fatalf("merging: %v", err)
	}
	if after.ID != surviving {
		t.Errorf("the merge returned %s, want the surviving incident %s", after.ID, surviving)
	}

	gone, err := database.Incident(context.Background(), organization, absorbed)
	if err != nil {
		t.Fatalf("the absorbed incident is unreadable after a merge: %v", err)
	}
	if gone.SupersededBy == nil || *gone.SupersededBy != surviving {
		t.Fatalf("the absorbed incident points at %v, want %s", gone.SupersededBy, surviving)
	}
	if gone.GroupingKey != "group-a" {
		t.Errorf("the absorbed incident's grouping key is now %q; a merge must rewrite nothing",
			gone.GroupingKey)
	}
	if !recordedIncidentMerge(t, database, organization, absorbed) {
		t.Error("no audit event names the merged incident; a grouping correction nobody can " +
			"attribute is one an auditor cannot answer for")
	}
}

func recordedIncidentMerge(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, incident uuid.UUID,
) bool {
	t.Helper()

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var count int
	if err = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_event
		 WHERE org_id = $1 AND action = 'incident.merged' AND target_id = $2`,
		organization, incident.String()).Scan(&count); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the audit trail: %v", err)
	}
	return count > 0
}

func alertmanagerIntegration(
	t *testing.T, database *storage.Database, organization uuid.UUID,
) uuid.UUID {
	t.Helper()

	created, err := database.CreateIntegration(
		context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			Provider:            "alertmanager",
			Name:                "alertmanager " + uuid.NewString(),
			WebhookSecretDigest: randomDigest(t),
		})
	if err != nil {
		t.Fatalf("creating an alertmanager integration: %v", err)
	}
	return created.ID
}

func TestTwoDeliveriesCarryingOneGroupAtOnce_ProduceOneIncidentAndBothSucceed(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)

	const key = "{}:{alertname=KubePodCrashLooping}"
	began := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	delivery := func(fingerprint string, body byte) storage.Delivery {
		digest := make([]byte, 32)
		digest[0] = body
		return storage.Delivery{
			Integration: integration,
			AlertDelivery: alertevent.AlertDelivery{
				ContentDigest: digest,
				AlertEvents: []alertevent.AlertEvent{{
					SourceKey:   fingerprint,
					GroupingKey: key,
					Status:      alertevent.AlertEventFiring,
					Title:       "KubePodCrashLooping",
					StartedAt:   began,
				}},
			},
		}
	}

	type answer struct {
		outcome storage.DeliveryOutcome
		err     error
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for index, fingerprint := range []string{"fp-one", "fp-two"} {
		go func() {
			<-start
			outcome, err := database.RecordDelivery(
				context.Background(), organization, delivery(fingerprint, byte(index+1)), storage.AlertAdmissionPolicy{})
			answers <- answer{outcome, err}
		}()
	}
	close(start)

	var opened, joined int
	for range 2 {
		got := <-answers
		if got.err != nil {
			t.Fatalf("a concurrent delivery failed: %v", got.err)
		}
		opened += got.outcome.IncidentsOpened
		joined += got.outcome.IncidentsJoined
	}
	if opened != 1 || joined != 1 {
		t.Errorf("two concurrent deliveries in one group opened %d incidents and joined %d, "+
			"want one of each", opened, joined)
	}

	page, err := database.QueryIncidents(context.Background(), organization, incident.Query{
		Sort: "lastSeenAt", Descending: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("reading the incidents: %v", err)
	}
	if len(page.Incidents) != 1 {
		t.Fatalf("two concurrent deliveries in one group produced %d incidents, want 1",
			len(page.Incidents))
	}
	if page.Incidents[0].AlertEventCount != 2 {
		t.Errorf("the incident holds %d alertEvents, want the 2 that were delivered",
			page.Incidents[0].AlertEventCount)
	}
	if page.Incidents[0].Basis != incident.BasisSourceGrouping {
		t.Errorf("the incident reports basis %v, want the source's own grouping",
			page.Incidents[0].Basis)
	}
	if _, _, found, err := database.ClaimInvestigation(context.Background(), aClaim("first")); err != nil || !found {
		t.Fatalf("concurrent deliveries left no automatic Investigation: found=%t err=%v", found, err)
	}
	if _, _, found, err := database.ClaimInvestigation(context.Background(), aClaim("second")); err != nil || found {
		t.Fatalf("concurrent deliveries left a second automatic Investigation: found=%t err=%v", found, err)
	}
}

func TestIncidentAlertEventCountIsDerivedFromAlertEvents(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	incidentID := recordIncident(t, database, organization, integration, "derived-count")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"one", "two"} {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO alert_event
				(alert_event_id, org_id, integration_id, source_key, status, title, summary,
				 started_at, incident_id, updated_at)
			VALUES ($1, $2, $3, $4, 1, 'Alert', 'Summary', $5, $6, now())`,
			uuid.New(), organization, integration, source, time.Now().UTC(), incidentID); err != nil {
			t.Fatal(err)
		}
	}
	found, err := database.Incident(context.Background(), organization, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if found.AlertEventCount != 2 {
		t.Fatalf("incident count = %d, want the two owned AlertEvents", found.AlertEventCount)
	}
}

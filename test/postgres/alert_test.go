package storage_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAlertBatchSharesCapacityWithManualConversationAndSlackProducers(t *testing.T) {
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
	work, found, err := database.ClaimSlackMessageWork(ctx, "slack-worker", time.Minute)
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
	go func() { slackResult <- database.ApplySlackMessageWork(ctx, organization, work, time.Hour, 3) }()
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
	if err := <-slackResult; !errors.Is(err, storage.ErrInvestigationCapacity) {
		t.Fatalf("Slack producer: %v", err)
	}
	for range 3 {
		if _, _, found, err := database.ClaimInvestigation(ctx, aClaim("alert")); err != nil || !found {
			t.Fatalf("claiming alert batch: found=%t err=%v", found, err)
		}
	}
	if err := database.ApplySlackMessageWork(ctx, organization, work, time.Hour, 3); err != nil {
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
	if _, found, err := database.ClaimSlackMessageWork(ctx, "unused-alert-worker", time.Minute); err != nil || found {
		t.Fatalf("new alert delivery queued webhook work: found=%t err=%v", found, err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	var deliveries, work int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM webhook_delivery WHERE org_id = $1),
		(SELECT count(*) FROM slack_message_work WHERE org_id = $1)`, organization).Scan(&deliveries, &work); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || work != 0 {
		t.Fatalf("accepted alert idempotency rows=%d slack work=%d", deliveries, work)
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

func TestFailureAtEveryAlertAcceptanceWriteStageRollsBackTheCompleteDelivery(t *testing.T) {
	for _, table := range []string{"webhook_delivery", "alert_event", "incident", "investigation"} {
		t.Run(table, func(t *testing.T) {
			database, organization := migratedDatabase(t)
			ctx := context.Background()
			pool, err := database.Pool(organization)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_alert_acceptance_stage()
				RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				RAISE EXCEPTION 'injected alert acceptance failure'; END $$`); err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER reject_alert_acceptance_stage
				BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION reject_alert_acceptance_stage()`, table)
			if _, err = pool.Exec(ctx, trigger); err != nil {
				t.Fatal(err)
			}

			delivery := alertInvestigationDelivery(alertmanagerIntegration(t, database, organization),
				alertInvestigationEvent("stage", "stage", "Stage failure", "2026-09-29T10:00:00Z"))
			if _, err = database.RecordDelivery(ctx, organization, delivery,
				storage.AlertAdmissionPolicy{}); err == nil {
				t.Fatalf("accepted delivery despite injected %s failure", table)
			}
			assertNoAlertDeliveryFacts(t, database, organization)

			if _, err = pool.Exec(ctx, fmt.Sprintf(
				"DROP TRIGGER reject_alert_acceptance_stage ON %s", table)); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, "DROP FUNCTION reject_alert_acceptance_stage()"); err != nil {
				t.Fatal(err)
			}
			outcome, err := database.RecordDelivery(ctx, organization, delivery,
				storage.AlertAdmissionPolicy{})
			if err != nil || outcome.Duplicate || outcome.IncidentsOpened != 1 {
				t.Fatalf("retry after %s failure: %+v, %v", table, outcome, err)
			}
		})
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

func TestAutomaticIncidentInvestigationIsUniqueAndManualWorkCanCoexist(t *testing.T) {
	database, organization, other := twoOrganizationsInOneDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	incident := recordIncident(t, database, organization, integration, "automatic")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	insert := func(automatic bool) error {
		_, err := pool.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, incident_id, subject, window_from, window_until,
			 automatic_incident)
			VALUES ($1, $2, $3, 'service', now(), now(), $4)`,
			uuid.New(), organization, incident, automatic)
		return err
	}
	if err := insert(true); err != nil {
		t.Fatalf("first automatic Investigation: %v", err)
	}
	var violation *pgconn.PgError
	if err := insert(true); !errors.As(err, &violation) || violation.Code != "23505" {
		t.Fatalf("second automatic Investigation error = %v, want unique violation", err)
	}
	for range 2 {
		if err := insert(false); err != nil {
			t.Fatalf("manual Investigation on the same Incident: %v", err)
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO investigation
		(investigation_id, org_id, incident_id, subject, window_from, window_until,
		 automatic_incident)
		VALUES ($1, $2, $3, 'service', now(), now(), true)`, uuid.New(), other, incident)
	if !errors.As(err, &violation) || violation.Code != "23503" {
		t.Fatalf("cross-Organization Incident error = %v, want foreign-key violation", err)
	}
}

func TestAutomaticIncidentInvestigationRequiresAnIncidentAndCannotBeAConversationTurn(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	incident := recordIncident(t, database, organization,
		alertmanagerIntegration(t, database, organization), "shape")
	chat := openConversation(t, database, organization, "service")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		incident     any
		conversation any
		turn         any
	}{
		{name: "missing Incident"},
		{name: "Conversation turn", incident: incident, conversation: chat.ID, turn: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), `INSERT INTO investigation
				(investigation_id, org_id, incident_id, conversation_id, turn,
				 subject, window_from, window_until, automatic_incident)
				VALUES ($1, $2, $3, $4, $5, 'service', now(), now(), true)`,
				uuid.New(), organization, test.incident, test.conversation, test.turn)
			var violation *pgconn.PgError
			if !errors.As(err, &violation) || violation.Code != "23514" {
				t.Fatalf("invalid automatic Investigation error = %v, want check violation", err)
			}
		})
	}
}

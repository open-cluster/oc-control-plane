package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRelayConflictStateRollsBackWhenItsAuditEventFails(t *testing.T) {
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event ADD CONSTRAINT reject_detection
		CHECK (action <> 'relay.session_conflict.detected')`); err != nil {
		t.Fatal(err)
	}
	if err = database.RecordSessionConflict(ctx, organization, registration, 2); err == nil {
		t.Fatal("conflict state committed without its Audit Event")
	}
	if conflict, readErr := database.SessionConflict(ctx, organization, registration); readErr != nil || !conflict.DetectedAt.IsZero() {
		t.Fatalf("failed detection changed current state: %+v %v", conflict, readErr)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event DROP CONSTRAINT reject_detection`); err != nil {
		t.Fatal(err)
	}
	if err = database.RecordSessionConflict(ctx, organization, registration, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event ADD CONSTRAINT reject_withdrawal
		CHECK (action <> 'relay.session_conflict.cleared')`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ClearSessionConflict(ctx, ownerOf(t, organization), organization, registration); err == nil {
		t.Fatal("conflict withdrawal committed without its Audit Event")
	}
	conflict, err := database.SessionConflict(ctx, organization, registration)
	if err != nil || conflict.DetectedAt.IsZero() || conflict.DistinctHosts != 2 {
		t.Fatalf("failed withdrawal changed current state: %+v %v", conflict, err)
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE audit_event DROP CONSTRAINT reject_withdrawal`); err != nil {
		t.Fatal(err)
	}
}

func recordAuditEvent(
	t *testing.T, dsn string, organization uuid.UUID, occurredAt time.Time,
) uuid.UUID {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to write an audit event: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	id := uuid.New()
	ensureOrganization(t, connection, organization)
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO audit_event
			(event_id, org_id, actor_kind, actor_display_name, action, target_kind,
			 target_id, outcome, occurred_at)
		VALUES ($1, $2, 3, 'the retention test', 'integration.revised', 'integration', $3, 1, $4)`,
		id, organization, id.String(), occurredAt); err != nil {
		t.Fatalf("writing an audit event: %v", err)
	}
	return id
}

func declareRetention(t *testing.T, dsn string, organization uuid.UUID, days int) {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to declare a retention schedule: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	if _, err = connection.Exec(context.Background(), `
		INSERT INTO organization (org_id, display_name, created_by, audit_retention_days)
		VALUES ($1::uuid, $1::text, 'retention-test', $2)
		ON CONFLICT (org_id) DO UPDATE SET audit_retention_days = EXCLUDED.audit_retention_days`,
		organization, days); err != nil {
		t.Fatalf("declaring a retention schedule: %v", err)
	}
}

func ensureOrganization(t *testing.T, connection *pgx.Conn, organization uuid.UUID) {
	t.Helper()
	if _, err := connection.Exec(context.Background(), `
		INSERT INTO organization (org_id, display_name, created_by)
		VALUES ($1::uuid, $1::text, 'retention-test')
		ON CONFLICT (org_id) DO NOTHING`, organization); err != nil {
		t.Fatalf("creating organization: %v", err)
	}
}

func countAuditEvents(t *testing.T, dsn string, organization uuid.UUID) int {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to count audit events: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	var count int
	if err = connection.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_event WHERE org_id = $1`,
		organization).Scan(&count); err != nil {
		t.Fatalf("counting audit events: %v", err)
	}
	return count
}

func TestPruneEventsBefore_RemovesWhatAgedOutAndKeepsWhatDidNot(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org := organization(t, "org-a")

	now := time.Now().UTC()
	aged := []uuid.UUID{
		recordAuditEvent(t, dsn, org, now.Add(-90*24*time.Hour)),
		recordAuditEvent(t, dsn, org, now.Add(-60*24*time.Hour)),
	}
	recordAuditEvent(t, dsn, org, now.Add(-2*time.Hour))
	recordAuditEvent(t, dsn, org, now)

	horizon := now.AddDate(0, 0, -30)
	removed, err := database.PruneEventsBefore(context.Background(), org, horizon, 1000)
	if err != nil {
		t.Fatalf("PruneEventsBefore: %v", err)
	}
	if removed != int64(len(aged)) {
		t.Errorf("pruning removed %d events, want the %d older than the horizon",
			removed, len(aged))
	}
	if remaining := countAuditEvents(t, dsn, org); remaining != 2 {
		t.Errorf("%d events remain, want the 2 inside the retention period", remaining)
	}
}

func TestPruneEventsBefore_RemovesNoMoreThanItWasAskedFor(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org := organization(t, "org-a")

	now := time.Now().UTC()
	for hour := range 5 {
		recordAuditEvent(t, dsn, org, now.Add(-time.Duration(90+hour)*24*time.Hour))
	}

	horizon := now.AddDate(0, 0, -30)
	removed, err := database.PruneEventsBefore(context.Background(), org, horizon, 2)
	if err != nil {
		t.Fatalf("PruneEventsBefore: %v", err)
	}
	if removed != 2 {
		t.Errorf("a batch of 2 removed %d events", removed)
	}
	if remaining := countAuditEvents(t, dsn, org); remaining != 3 {
		t.Errorf("%d events remain after one bounded batch, want 3", remaining)
	}
}

func TestPruneEventsBefore_TouchesNoOtherTenantsRecord(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	mine, theirs := organization(t, "org-a"), organization(t, "org-b")

	now := time.Now().UTC()
	recordAuditEvent(t, dsn, mine, now.Add(-90*24*time.Hour))
	recordAuditEvent(t, dsn, theirs, now.Add(-90*24*time.Hour))

	if _, err := database.PruneEventsBefore(
		context.Background(), mine, now.AddDate(0, 0, -30), 1000); err != nil {
		t.Fatalf("PruneEventsBefore: %v", err)
	}
	if remaining := countAuditEvents(t, dsn, theirs); remaining != 1 {
		t.Errorf("another tenant's record lost %d events to a schedule that was not theirs",
			1-remaining)
	}
}

func TestTheRecordIsDeletableOnlyInsideATransactionThatDeclaresItselfThePruner(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org := organization(t, "org-a")

	now := time.Now().UTC()
	recordAuditEvent(t, dsn, org, now.Add(-90*24*time.Hour))
	recordAuditEvent(t, dsn, org, now.Add(-91*24*time.Hour))

	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	undeclared := func(when string) {
		t.Helper()
		if _, execErr := pool.Exec(context.Background(),
			`DELETE FROM audit_event WHERE org_id = $1`, org); execErr == nil {
			t.Fatalf("an undeclared DELETE succeeded %s; the record is not append-only", when)
		}
	}

	undeclared("before the pruner ran")

	removed, err := database.PruneEventsBefore(
		context.Background(), org, now.AddDate(0, 0, -30), 1000)
	if err != nil {
		t.Fatalf("PruneEventsBefore: %v", err)
	}
	if removed != 2 {
		t.Fatalf("the declared prune removed %d events, want 2", removed)
	}

	recordAuditEvent(t, dsn, org, now.Add(-92*24*time.Hour))
	for range 8 {
		undeclared("after the pruner ran")
	}
	if remaining := countAuditEvents(t, dsn, org); remaining != 1 {
		t.Errorf("%d events remain; the undeclared deletes above should have changed nothing",
			remaining)
	}
}

func TestDeclaredRetentions_ReportsOnlyTheTenantsThatDeclaredASchedule(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	declareRetention(t, dsn, organization(t, "org-a"), 30)
	declareRetention(t, dsn, organization(t, "org-quiet"), 0)
	declareRetention(t, dsn, organization(t, "org-far"), 7)

	declared, err := database.DeclaredRetentions(context.Background())
	if err != nil {
		t.Fatalf("DeclaredRetentions: %v", err)
	}

	days := make(map[string]int, len(declared))
	for _, one := range declared {
		days[one.Organization.String()] = one.Days
	}
	orgA := organization(t, "org-a").String()
	if days[orgA] != 30 {
		t.Errorf("org-a declared 30 days and is reported as %d", days[orgA])
	}
	orgFar := organization(t, "org-far").String()
	if days[orgFar] != 7 {
		t.Errorf("a second tenant declared 7 days and is reported as %d",
			days[orgFar])
	}
	if _, reported := days[organization(t, "org-quiet").String()]; reported {
		t.Error("a tenant declaring zero days was reported; zero keeps everything, and acting " +
			"on it would delete a whole record")
	}
}

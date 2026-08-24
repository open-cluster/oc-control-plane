package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Applying the retention schedule a tenant declared, through the one path the database permits.
//
// The record is append-only and enforced as such by the database: an UPDATE, a DELETE and a
// TRUNCATE are all refused, EXCEPT in a transaction that has declared itself the pruner. So the
// assertions that matter are not "rows went" — they are that rows go ONLY through that path, and
// that the declaration does not outlive the transaction that made it.

// recordAuditEvent writes one event directly.
//
// It is raw SQL rather than the application's own writer because the writer commits an event
// alongside a change it is describing, and what is under test here has no change to describe. The
// INSERT is not what the trigger guards, so this reaches the same rows by the same door.
func recordAuditEvent(
	t *testing.T, dsn string, organization tenancy.Organization, occurredAt time.Time,
) uuid.UUID {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to write an audit event: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	id := uuid.New()
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO audit_event
			(event_id, org_id, actor_kind, actor_display_name, action, target_kind,
			 target_id, outcome, occurred_at)
		VALUES ($1, $2, 3, 'the retention test', 'integration.revised', 'integration', $3, 1, $4)`,
		id, organization.String(), id.String(), occurredAt); err != nil {
		t.Fatalf("writing an audit event: %v", err)
	}
	return id
}

func declareRetention(t *testing.T, dsn string, organization tenancy.Organization, days int) {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to declare a retention schedule: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	if _, err = connection.Exec(context.Background(), `
		INSERT INTO organization_policy (org_id, audit_retention_days)
		VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET audit_retention_days = EXCLUDED.audit_retention_days`,
		organization.String(), days); err != nil {
		t.Fatalf("declaring a retention schedule: %v", err)
	}
}

func countAuditEvents(t *testing.T, dsn string, organization tenancy.Organization) int {
	t.Helper()

	connection, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to count audit events: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	var count int
	if err = connection.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_event WHERE org_id = $1`,
		organization.String()).Scan(&count); err != nil {
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

// The bound is what keeps a first sweep against years of history from being one lock somebody
// notices as an outage. A short batch is also how the pruner knows the backlog is gone.
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

// One tenant's schedule never reaches another tenant's record, even on the same database.
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

// THE PROPERTY THE WHOLE MECHANISM RESTS ON.
//
// The pruner declares itself with a setting local to its transaction. A SESSION-level setting
// would survive on a pooled connection and turn every later transaction that happened to get it
// into one permitted to delete the record — which would make an append-only guarantee depend on
// connection assignment. So: an ordinary delete is refused, a declared one succeeds, and an
// ordinary delete afterwards is refused again.
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

	pool, err := database.Pool(org)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	undeclared := func(when string) {
		t.Helper()
		if _, execErr := pool.Exec(context.Background(),
			`DELETE FROM audit_event WHERE org_id = $1`, org.String()); execErr == nil {
			t.Fatalf("an undeclared DELETE succeeded %s; the record is not append-only", when)
		}
	}

	undeclared("before the pruner ran")

	// The pool is small and reused deliberately: the assertion below is only meaningful if the
	// connection the pruner declared on can come back to a later caller, which is exactly the
	// leak a session-level setting would produce.
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

// A tenant that declared nothing is not reported, and treating its zero as a horizon of "now"
// would delete an entire record because somebody never set a policy.
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
	if days["org-a"] != 30 {
		t.Errorf("org-a declared 30 days and is reported as %d", days["org-a"])
	}
	// Every Organization in the deployment database is scanned.
	if days["org-far"] != 7 {
		t.Errorf("a second tenant declared 7 days and is reported as %d",
			days["org-far"])
	}
	if _, reported := days["org-quiet"]; reported {
		t.Error("a tenant declaring zero days was reported; zero is the product default of " +
			"keeping everything, and acting on it would delete a whole record")
	}
}

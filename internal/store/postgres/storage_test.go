package storage_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// postgresDSN starts one Postgres and returns a DSN for the default database. Integration
// tests share nothing but the server; each test creates its own database so they stay
// independent and parallel-safe.
func postgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("controlplane"),
		tcpostgres.WithUsername("controlplane"),
		tcpostgres.WithPassword("controlplane"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		noContainerRuntime(t, err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func openDatabaseForTest(t *testing.T, dsn string) *storage.Database {
	t.Helper()
	opened, err := storage.OpenDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(opened.Close)
	return opened
}

func organization(t *testing.T, id string) tenancy.Organization {
	t.Helper()
	value, err := tenancy.NewOrganization(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	return value
}

func TestOpenDatabase_MigratesOnceAndBecomesReady(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database, err := storage.OpenDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(database.Close)

	applied, err := database.Migrate(context.Background())
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("the first run must apply at least one migration")
	}
	again, err := database.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second run must apply nothing, applied %v", again)
	}
	if err := database.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestMigrate_AppliesEveryMigrationThenIsIdempotent(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)

	applied, err := database.Migrate(context.Background())
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("the first run must apply at least one migration")
	}

	again, err := database.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second run must apply nothing, applied %v", again)
	}
}

// The schema the migrations describe must actually exist afterwards; recording a version
// without applying its statements would satisfy the ledger and nothing else.
func TestMigrate_SchemaIsUsableAfterwards(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := database.Pool(organization(t, "org-a"))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organization_policy (org_id, audit_retention_days) VALUES ($1, $2)`,
		"org-a", 30); err != nil {
		t.Fatalf("the migrated schema must accept a tenant row: %v", err)
	}

	var days int
	if err := pool.QueryRow(ctx,
		`SELECT audit_retention_days FROM organization_policy WHERE org_id = $1`,
		"org-a").Scan(&days); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if days != 30 {
		t.Errorf("audit_retention_days = %d, want the 30 that was written", days)
	}
}

func TestMigrate_AddsGenericWebhookDeliveryIdentity(t *testing.T) {
	t.Parallel()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := database.Pool(organization(t, "schema-check"))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var key string
	if err := pool.QueryRow(context.Background(),
		`SELECT key FROM integration_type WHERE integration_type_id = 5`).Scan(&key); err != nil {
		t.Fatalf("generic webhook seed: %v", err)
	}
	if key != "generic_webhook" {
		t.Errorf("integration type 5 key = %q, want generic_webhook", key)
	}

	var columns int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'integration_delivery'
		   AND column_name IN ('provider_identity', 'lifecycle_phase', 'request_id')`).Scan(&columns); err != nil {
		t.Fatalf("delivery identity columns: %v", err)
	}
	if columns != 3 {
		t.Errorf("delivery identity column count = %d, want 3", columns)
	}
}

func TestMigrate_BackfillsEveryHistoricalAcceptedDeliveryWithoutChangingHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	for _, name := range []string{"0001_baseline.sql", "0002_organization.sql"} {
		body, readErr := os.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if _, err = connection.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration
		(
			version text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO schema_migration (version) VALUES ('0001_baseline'), ('0002_organization');
		INSERT INTO organization (org_id, created_by) VALUES ('upgrade-org', 'migration test');
		INSERT INTO integration (integration_id, org_id, integration_type_id, name) VALUES
			('00000000-0000-0000-0000-000000000101', 'upgrade-org', 1, 'Historical Alertmanager'),
			('00000000-0000-0000-0000-000000000104', 'upgrade-org', 3, 'Historical Slack');
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, reason) VALUES
			('10000000-0000-0000-0000-000000000001', 'upgrade-org',
			 '00000000-0000-0000-0000-000000000101', 1, decode(repeat('a1', 32), 'hex'), ''),
			('10000000-0000-0000-0000-000000000002', 'upgrade-org',
			 '00000000-0000-0000-0000-000000000104', 1, decode(repeat('b2', 32), 'hex'), ''),
			('10000000-0000-0000-0000-000000000003', 'upgrade-org',
			 '00000000-0000-0000-0000-000000000101', 2, NULL, ''),
			('10000000-0000-0000-0000-000000000004', 'upgrade-org',
			 '00000000-0000-0000-0000-000000000104', 3, NULL, 'unauthorized')`); err != nil {
		t.Fatalf("seed pre-0003 history: %v", err)
	}
	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if len(applied) != 1 || applied[0] != "0003_generic_webhook_delivery_identity" {
		t.Fatalf("upgrade applied %v, want only 0003", applied)
	}
	rows, err := connection.Query(ctx, `
		SELECT outcome, encode(body_digest, 'hex'), provider_identity, lifecycle_phase
		  FROM integration_delivery ORDER BY delivery_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found int
	for rows.Next() {
		var outcome int
		var digest, identity, phase *string
		if err = rows.Scan(&outcome, &digest, &identity, &phase); err != nil {
			t.Fatal(err)
		}
		found++
		if outcome == 1 {
			if digest == nil || identity == nil || *identity != *digest || phase == nil || *phase != "" {
				t.Errorf("accepted history was not safely backfilled: digest=%v identity=%v phase=%v",
					digest, identity, phase)
			}
		} else if identity != nil || phase != nil {
			t.Errorf("nonaccepted outcome %d gained identity=%v phase=%v", outcome, identity, phase)
		}
	}
	if err = rows.Err(); err != nil || found != 4 {
		t.Fatalf("preserved history count=%d error=%v", found, err)
	}
	if _, err = connection.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, provider_identity, lifecycle_phase)
		VALUES (gen_random_uuid(), 'upgrade-org', '00000000-0000-0000-0000-000000000101',
			1, decode(repeat('c3', 32), 'hex'), repeat('a1', 32), '')`); err == nil {
		t.Fatal("the upgraded identity index accepted a conflicting historical identity")
	}
	if _, err = connection.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, provider_identity, lifecycle_phase)
		VALUES (gen_random_uuid(), 'upgrade-org', '00000000-0000-0000-0000-000000000101',
			1, decode(repeat('c3', 32), 'hex'), repeat('a1', 32), 'resolved')`); err != nil {
		t.Fatalf("the upgraded identity index refused a distinct lifecycle phase: %v", err)
	}
	if _, err = connection.Exec(ctx, `
		INSERT INTO integration_delivery
			(delivery_id, org_id, integration_id, outcome, provider_identity, lifecycle_phase)
		VALUES (gen_random_uuid(), 'upgrade-org', '00000000-0000-0000-0000-000000000101',
			2, 'must-not-survive', '')`); err == nil {
		t.Fatal("the upgraded constraints accepted identity on a duplicate attempt")
	}
}

func TestMigrationCountIncludesGenericWebhookDeliveryIdentity(t *testing.T) {
	t.Parallel()
	if got := storage.MigrationCount(); got < 3 {
		t.Errorf("MigrationCount() = %d, want at least 3", got)
	}
}

// Two instances starting at once must not both apply the same migration. The lock is what
// makes a rolling deployment safe; without it the second instance races the first into a
// partially applied schema.
func TestMigrate_ConcurrentInstancesApplyExactlyOnce(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	const instances = 4
	appliedCounts := make([]int, instances)
	failures := make([]error, instances)

	var start sync.WaitGroup
	var finished sync.WaitGroup
	start.Add(1)

	for index := range instances {
		database := openDatabaseForTest(t, dsn)

		finished.Add(1)
		go func() {
			defer finished.Done()
			start.Wait()
			applied, err := database.Migrate(context.Background())
			appliedCounts[index] = len(applied)
			failures[index] = err
		}()
	}

	start.Done()
	finished.Wait()

	total := 0
	for index, err := range failures {
		if err != nil {
			t.Fatalf("instance %d failed: %v", index, err)
		}
		total += appliedCounts[index]
	}

	expected := storage.MigrationCount()
	if expected == 0 {
		t.Fatal("no migrations are embedded; the exactly-once assertion would pass vacuously")
	}
	if total != expected {
		t.Errorf("across %d concurrent instances %d migrations were applied, want exactly %d",
			instances, total, expected)
	}
}

func TestPool_ReturnsTheDatabaseForAnOrganization(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)

	pool, err := database.Pool(organization(t, "org-a"))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if pool == nil {
		t.Fatal("Pool must return a usable pool")
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Errorf("the resolved pool must be usable: %v", err)
	}
}

func TestPool_ZeroOrganizationIsRefused(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	database := openDatabaseForTest(t, dsn)

	var zero tenancy.Organization
	if _, err := database.Pool(zero); err == nil {
		t.Fatal("the zero Organization must never resolve to a pool")
	}
}

func TestPool_QueriesProduceASpanBeneathTheCaller(t *testing.T) {
	dsn := postgresDSN(t)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	database := openDatabaseForTest(t, dsn)
	pool, err := database.Pool(organization(t, "org-a"))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}

	ctx, parent := provider.Tracer("test").Start(context.Background(), "handle request")
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	parent.End()

	var sawChild bool
	for _, span := range recorder.Ended() {
		if span.Parent().SpanID() == parent.SpanContext().SpanID() &&
			span.SpanContext().TraceID() == parent.SpanContext().TraceID() {
			sawChild = true
		}
	}
	if !sawChild {
		t.Error("the database query produced no span beneath the caller's span")
	}
}

// ownerOf is the principal these tests act as: an Admin of the Organization under test.
func ownerOf(t *testing.T, organization tenancy.Organization) authz.Principal {
	t.Helper()
	return memberOf(t, organization, authz.Admin)
}

// memberOf builds a principal holding one role in one Organization.
func memberOf(
	t *testing.T, organization tenancy.Organization, role authz.Role,
) authz.Principal {
	t.Helper()

	principal, err := authz.NewPrincipal(authz.KindUser, "user-under-test", "Test Operator",
		[]authz.Membership{{Organization: organization, Role: role}})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

// aStranger is a principal who holds a role in another Organization.
func aStranger(t *testing.T) authz.Principal {
	t.Helper()

	elsewhere, err := tenancy.NewOrganization("org-somebody-else")
	if err != nil {
		t.Fatalf("naming another organization: %v", err)
	}
	return memberOf(t, elsewhere, authz.Admin)
}

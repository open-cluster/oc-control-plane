package storage_test

import (
	"context"
	"errors"
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

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
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

// databaseNamed creates a fresh database on the same server and returns its DSN, so one
// container can back several independent placements.
func databaseNamed(t *testing.T, adminDSN, name string) string {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	if _, err := connection.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	return replaceDatabase(adminDSN, name)
}

// replaceDatabase swaps the database segment of a DSN of the form
// postgres://user:pw@host:port/db?params.
func replaceDatabase(dsn, name string) string {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		panic(err)
	}
	config.Database = name
	return "postgres://" + config.User + ":" + config.Password +
		"@" + config.Host + ":" + itoa(int(config.Port)) + "/" + name + "?sslmode=disable"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func openPlacements(t *testing.T, placements, assignments map[string]string) *storage.Placements {
	t.Helper()
	return openLayout(t, storage.Layout{Placements: placements, Assignments: assignments})
}

func openLayout(t *testing.T, layout storage.Layout) *storage.Placements {
	t.Helper()

	opened, err := storage.OpenPlacements(context.Background(), layout)
	if err != nil {
		t.Fatalf("OpenPlacements: %v", err)
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

func TestMigrate_AppliesEveryMigrationThenIsIdempotent(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})

	applied, err := placements.Migrate(context.Background())
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if len(applied["shared"]) == 0 {
		t.Fatal("the first run must apply at least one migration")
	}

	again, err := placements.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again["shared"]) != 0 {
		t.Errorf("the second run must apply nothing, applied %v", again["shared"])
	}
}

// The schema the migrations describe must actually exist afterwards; recording a version
// without applying its statements would satisfy the ledger and nothing else.
func TestMigrate_SchemaIsUsableAfterwards(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := placements.Pool(organization(t, "org-a"))
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
		placements := openPlacements(t,
			map[string]string{"shared": dsn},
			map[string]string{"org-a": "shared"})

		finished.Add(1)
		go func() {
			defer finished.Done()
			start.Wait()
			applied, err := placements.Migrate(context.Background())
			appliedCounts[index] = len(applied["shared"])
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

func TestPool_ResolvesAnAssignedOrganization(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})

	pool, err := placements.Pool(organization(t, "org-a"))
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

// The core isolation property: two organizations on different placements reach different
// databases.
func TestPool_DifferentPlacementsReachDifferentDatabases(t *testing.T) {
	t.Parallel()
	adminDSN := postgresDSN(t)
	dedicatedDSN := databaseNamed(t, adminDSN, "acme")

	placements := openPlacements(t,
		map[string]string{"shared": adminDSN, "dedicated": dedicatedDSN},
		map[string]string{"org-shared": "shared", "org-acme": "dedicated"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ctx := context.Background()
	sharedPool, err := placements.Pool(organization(t, "org-shared"))
	if err != nil {
		t.Fatalf("Pool(org-shared): %v", err)
	}
	acmePool, err := placements.Pool(organization(t, "org-acme"))
	if err != nil {
		t.Fatalf("Pool(org-acme): %v", err)
	}

	if _, err := sharedPool.Exec(ctx,
		`INSERT INTO organization_policy (org_id, audit_retention_days) VALUES ($1, $2)`,
		"org-shared", 30); err != nil {
		t.Fatalf("write to shared: %v", err)
	}

	// The row written to the shared placement must not be visible from the dedicated one.
	var visible int
	if err := acmePool.QueryRow(ctx,
		`SELECT count(*) FROM organization_policy WHERE org_id = $1`,
		"org-shared").Scan(&visible); err != nil {
		t.Fatalf("read from dedicated: %v", err)
	}
	if visible != 0 {
		t.Error("a row written to one placement must not be readable from another")
	}

	var databaseName string
	if err := acmePool.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if databaseName != "acme" {
		t.Errorf("the dedicated placement resolved to database %q, want acme", databaseName)
	}
}

// An organization with no assignment must be a typed error. Falling back to a default
// connection is how one tenant is served another tenant's data.
func TestPool_UnassignedOrganizationIsATypedErrorNotAFallback(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})

	pool, err := placements.Pool(organization(t, "org-unknown"))
	if !errors.Is(err, storage.ErrUnknownOrganization) {
		t.Fatalf("error = %v, want ErrUnknownOrganization", err)
	}
	if pool != nil {
		t.Fatal("no pool may be returned for an unresolvable organization")
	}
}

func TestPool_ZeroOrganizationIsRefused(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})

	var zero tenancy.Organization
	if _, err := placements.Pool(zero); err == nil {
		t.Fatal("the zero Organization must never resolve to a pool")
	}
}

func TestOpenPlacements_RefusesAnAssignmentToAnUndefinedPlacement(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	_, err := storage.OpenPlacements(context.Background(), storage.Layout{
		Placements:  map[string]string{"shared": dsn},
		Assignments: map[string]string{"org-a": "nowhere"},
	})
	if err == nil {
		t.Fatal("an assignment naming an undefined placement must fail at open")
	}
}

func TestOpenPlacements_RefusesAnUndefinedDefaultPlacement(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	_, err := storage.OpenPlacements(context.Background(), storage.Layout{
		Placements:       map[string]string{"shared": dsn},
		DefaultPlacement: "nowhere",
	})
	if err == nil {
		t.Fatal("a default naming an undefined placement must fail at open")
	}
}

// The shared tier: an organization nobody enumerated still resolves, because enumerating
// five thousand of them in configuration is not a deployment.
func TestPool_UnassignedOrganizationUsesTheDefaultPlacement(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openLayout(t, storage.Layout{
		Placements:       map[string]string{"shared": dsn},
		DefaultPlacement: "shared",
	})

	pool, err := placements.Pool(organization(t, "never-heard-of-this-org"))
	if err != nil {
		t.Fatalf("an unassigned organization must reach the default placement: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Errorf("the default pool must be usable: %v", err)
	}
}

// An explicit assignment beats the default; that is how a Business or Enterprise tenant
// gets a dedicated database while everyone else shares one.
func TestPool_AssignmentOverridesTheDefault(t *testing.T) {
	t.Parallel()
	adminDSN := postgresDSN(t)
	dedicatedDSN := databaseNamed(t, adminDSN, "acme")

	placements := openLayout(t, storage.Layout{
		Placements:       map[string]string{"shared": adminDSN, "dedicated": dedicatedDSN},
		Assignments:      map[string]string{"org-acme": "dedicated"},
		DefaultPlacement: "shared",
	})

	acmePool, err := placements.Pool(organization(t, "org-acme"))
	if err != nil {
		t.Fatalf("Pool(org-acme): %v", err)
	}

	var databaseName string
	if err := acmePool.QueryRow(context.Background(),
		`SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if databaseName != "acme" {
		t.Errorf("the assigned organization reached %q, want its dedicated database", databaseName)
	}
}

// One dedicated tenant's database being unreachable must NOT withdraw the instance from
// service for every other tenant. Readiness asks whether this instance can serve at all,
// not whether every tenant is currently healthy.
func TestPing_OneDedicatedPlacementDownDoesNotMakeTheInstanceUnready(t *testing.T) {
	t.Parallel()
	adminDSN := postgresDSN(t)

	placements := openLayout(t, storage.Layout{
		Placements: map[string]string{
			"shared":    adminDSN,
			"dedicated": replaceDatabase(adminDSN, "does-not-exist"),
		},
		Assignments:      map[string]string{"org-acme": "dedicated"},
		DefaultPlacement: "shared",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := placements.Ping(ctx); err != nil {
		t.Errorf("one dedicated placement being down must not make the instance unready: %v", err)
	}
}

// A query must produce a span descending from the caller's, or a request trace stops at
// the handler and the database is invisible in it.
// Not parallel: the pgx instrumentation resolves its tracer from the GLOBAL provider, which
// is what production does (observability.Start installs it), so this test must install one
// too rather than pass a local provider in through a seam that would exist only for tests.
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

	placements := openLayout(t, storage.Layout{
		Placements:       map[string]string{"shared": dsn},
		DefaultPlacement: "shared",
	})
	pool, err := placements.Pool(organization(t, "org-a"))
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

func TestPing_ReportsReachabilityOfEveryPlacement(t *testing.T) {
	t.Parallel()
	dsn := postgresDSN(t)

	placements := openPlacements(t,
		map[string]string{"shared": dsn},
		map[string]string{"org-a": "shared"})

	if err := placements.Ping(context.Background()); err != nil {
		t.Fatalf("a reachable placement must ping: %v", err)
	}

	unreachable, err := storage.OpenPlacements(context.Background(), storage.Layout{
		Placements:  map[string]string{"gone": replaceDatabase(dsn, "does-not-exist")},
		Assignments: map[string]string{"org-a": "gone"},
	})
	if err != nil {
		// Refusing at open is an acceptable stricter behaviour.
		return
	}
	t.Cleanup(unreachable.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := unreachable.Ping(ctx); err == nil {
		t.Error("an unreachable placement must fail Ping so readiness reports unready")
	}
}

// ownerOf is the principal these tests act as: an owner of the organization under test.
//
// Every operator-facing store function takes one, because the tenancy boundary is checked here
// as well as in the authorization middleware. That duplication is what the boundary tests below
// exercise: a call made from a path nobody routed through the middleware is still refused.
func ownerOf(t *testing.T, organization tenancy.Organization) authz.Principal {
	t.Helper()
	return memberOf(t, organization, authz.Admin)
}

// memberOf builds a principal holding one role in one organization.
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

// aStranger is a principal who holds a role somewhere else entirely. It is what the boundary
// tests present to prove that a store function refuses a caller with no membership, rather than
// proving only that a caller with one is served.
func aStranger(t *testing.T) authz.Principal {
	t.Helper()

	elsewhere, err := tenancy.NewOrganization("org-somebody-else")
	if err != nil {
		t.Fatalf("naming another organization: %v", err)
	}
	return memberOf(t, elsewhere, authz.Admin)
}

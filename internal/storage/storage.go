// Package storage owns every database connection the control plane makes. It resolves an
// organization to its placement — where a tenant's data lives is looked up from the
// organization and is never ambient — holds one pool per placement, and applies the
// embedded migrations under a lock so concurrently starting instances cannot race the
// schema.
//
// No other package constructs a database connection. That is enforced by the import gates
// in internal/gates, not by convention, because a connection built elsewhere is a
// connection that bypasses placement resolution.
package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"sort"
	"strings"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is the advisory-lock key the migration runner serialises on. It is an
// arbitrary constant; it only has to be stable across releases and unused elsewhere.
const migrationLockKey int64 = 7_263_041_998_120_001

// ErrUnknownOrganization reports an organization with no placement assignment. It is
// deliberately a hard error: serving such a caller from a default connection is how one
// tenant is handed another tenant's data.
var ErrUnknownOrganization = errors.New("organization has no placement assignment")

// Layout describes where organizations live. It travels as one value because the three
// fields are meaningless apart: an assignment names a placement, and a default must be one.
type Layout struct {
	// Placements maps a placement name to its connection string.
	Placements map[string]string
	// Assignments maps an organization to a placement, overriding the default.
	Assignments map[string]string
	// DefaultPlacement serves unassigned organizations. Empty means there is none.
	DefaultPlacement string
}

// Placements holds one connection pool per placement and resolves organizations to them.
type Placements struct {
	pools       map[string]*pgxpool.Pool
	assignments map[string]string
	// defaultPlacement serves organizations with no explicit assignment. Empty means there
	// is none, and an unassigned organization is then a hard error.
	defaultPlacement string
}

// OpenPlacements builds a pool for every placement and validates that every assignment
// names a placement that exists. A typo in an assignment fails here, at startup, rather
// than becoming an unresolvable organization at request time.
//
// Opening a pool does not connect; pgxpool dials lazily. Reachability is a readiness
// question, answered by Ping, not a startup question — a control plane that refuses to
// start because a database is briefly unavailable cannot report why.
func OpenPlacements(ctx context.Context, layout Layout) (*Placements, error) {
	if len(layout.Placements) == 0 {
		return nil, errors.New("storage: at least one placement is required")
	}

	for organization, placement := range layout.Assignments {
		if _, ok := layout.Placements[placement]; !ok {
			return nil, fmt.Errorf(
				"storage: organization %q is assigned to undefined placement %q",
				organization, placement)
		}
	}
	if layout.DefaultPlacement != "" {
		if _, ok := layout.Placements[layout.DefaultPlacement]; !ok {
			return nil, fmt.Errorf("storage: default placement %q is not defined",
				layout.DefaultPlacement)
		}
	}

	placements, assignments := layout.Placements, layout.Assignments
	opened := &Placements{
		pools:            make(map[string]*pgxpool.Pool, len(placements)),
		assignments:      make(map[string]string, len(assignments)),
		defaultPlacement: layout.DefaultPlacement,
	}
	for name, dsn := range placements {
		poolConfig, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			opened.Close()
			// The DSN carries a password, and pgx quotes it on parse failure. Report the
			// placement by name and drop the cause.
			return nil, fmt.Errorf("storage: placement %q has an unusable connection string", name)
		}
		// Every query becomes a span, so a request trace reaches the database rather than
		// stopping at the handler. Statement text is recorded; arguments are not, because
		// they carry tenant data.
		poolConfig.ConnConfig.Tracer = otelpgx.NewTracer(
			otelpgx.WithTrimSQLInSpanName(),
			otelpgx.WithDisableQuerySpanNamePrefix(),
		)

		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			opened.Close()
			return nil, fmt.Errorf("storage: placement %q could not be opened", name)
		}
		opened.pools[name] = pool
	}
	maps.Copy(opened.assignments, assignments)
	return opened, nil
}

// Pool returns the connection pool for the organization's placement: its explicit
// assignment if it has one, otherwise the configured default. With no assignment and no
// default the organization is unresolvable, and that is an error rather than a guess.
func (p *Placements) Pool(organization tenancy.Organization) (*pgxpool.Pool, error) {
	if organization.IsEmpty() {
		return nil, fmt.Errorf("%w: the empty organization names no tenant", ErrUnknownOrganization)
	}
	placement, assigned := p.assignments[organization.String()]
	if !assigned {
		if p.defaultPlacement == "" {
			return nil, fmt.Errorf("%w: %s", ErrUnknownOrganization, organization)
		}
		placement = p.defaultPlacement
	}
	pool, ok := p.pools[placement]
	if !ok {
		// Unreachable while OpenPlacements validates assignments, and still not a fallback.
		return nil, fmt.Errorf("%w: placement %q is not open", ErrUnknownOrganization, placement)
	}
	return pool, nil
}

// Ping reports whether this instance can serve at all, and is what readiness consults.
//
// It deliberately does NOT require every placement to be reachable. Once a Business or
// Enterprise tenant has a dedicated database, an all-must-be-up check would let that one
// tenant's outage mark the instance unready and withdraw it from service for every other
// tenant — converting one customer's incident into everyone's. A dedicated placement being
// down degrades that tenant, which surfaces as their request failing, not as this
// instance's removal.
//
// With a default placement configured, that placement is what the instance must have: it
// serves every unassigned organization. With no default, every organization is named
// explicitly, the deployment is small by construction, and all placements are required.
func (p *Placements) Ping(ctx context.Context) error {
	required := p.names()
	if p.defaultPlacement != "" {
		required = []string{p.defaultPlacement}
	}
	for _, name := range required {
		if err := p.pools[name].Ping(ctx); err != nil {
			return fmt.Errorf("placement %q is unreachable: %w", name, err)
		}
	}
	return nil
}

// Close releases every pool. It is safe to call on a partially opened Placements.
func (p *Placements) Close() {
	for _, pool := range p.pools {
		if pool != nil {
			pool.Close()
		}
	}
}

// Migrate applies pending migrations to every placement and reports, per placement, the
// versions this call applied. An already-current placement reports none.
func (p *Placements) Migrate(ctx context.Context) (map[string][]string, error) {
	pending, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	applied := make(map[string][]string, len(p.pools))
	for _, name := range p.names() {
		versions, migrateErr := migratePlacement(ctx, p.pools[name], pending)
		if migrateErr != nil {
			return nil, fmt.Errorf("placement %q: %w", name, migrateErr)
		}
		applied[name] = versions
	}
	return applied, nil
}

// names returns placement names in a stable order so migration and ping order is
// deterministic across runs and across instances.
func (p *Placements) names() []string {
	names := make([]string, 0, len(p.pools))
	for name := range p.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MigrationCount reports how many migrations this binary carries.
func MigrationCount() int {
	migrations, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(migrations)
}

type migration struct {
	version    string
	statements string
}

// loadMigrations reads the embedded migrations in lexical order. Filenames are
// zero-padded, so lexical order is version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	migrations := make([]migration, 0, len(names))
	for _, name := range names {
		body, readErr := migrationFiles.ReadFile("migrations/" + name)
		if readErr != nil {
			return nil, fmt.Errorf("reading migration %s: %w", name, readErr)
		}
		migrations = append(migrations, migration{
			version:    strings.TrimSuffix(name, ".sql"),
			statements: string(body),
		})
	}
	return migrations, nil
}

// migratePlacement applies pending migrations inside ONE transaction holding a
// transaction-scoped advisory lock. Postgres has transactional DDL, so either every
// pending migration and its ledger row commit together or none do — a half-applied schema
// is not reachable. Concurrent instances serialise on the lock and the loser observes the
// winner's ledger rather than racing it.
func migratePlacement(
	ctx context.Context, pool *pgxpool.Pool, migrations []migration,
) (applied []string, err error) {
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			// Rollback on the failure path only; a committed transaction rolls back to a
			// no-op error that would otherwise mask the real one.
			_ = transaction.Rollback(ctx)
		}
	}()

	// The lock is taken before the ledger is read or created, so two instances cannot both
	// observe an empty ledger.
	if _, err = transaction.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquiring the migration lock: %w", err)
	}

	if _, err = transaction.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration
		(
			version    TEXT        NOT NULL PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("creating the migration ledger: %w", err)
	}

	present, err := appliedVersions(ctx, transaction)
	if err != nil {
		return nil, err
	}

	for _, pending := range migrations {
		if _, done := present[pending.version]; done {
			continue
		}
		if _, err = transaction.Exec(ctx, pending.statements); err != nil {
			return nil, fmt.Errorf("applying %s: %w", pending.version, err)
		}
		if _, err = transaction.Exec(ctx,
			`INSERT INTO schema_migration (version) VALUES ($1)`, pending.version); err != nil {
			return nil, fmt.Errorf("recording %s: %w", pending.version, err)
		}
		applied = append(applied, pending.version)
	}

	if err = transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return applied, nil
}

func appliedVersions(ctx context.Context, transaction pgx.Tx) (map[string]struct{}, error) {
	rows, err := transaction.Query(ctx, `SELECT version FROM schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("reading the migration ledger: %w", err)
	}
	defer rows.Close()

	present := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scanning the migration ledger: %w", err)
		}
		present[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the migration ledger: %w", err)
	}
	return present, nil
}

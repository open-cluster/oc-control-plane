// Package storage owns the control plane's single PostgreSQL connection pool and every
// query made through it. Organization remains an explicit argument and predicate on
// tenant-owned data; one database does not weaken tenant isolation.
//
// No other package constructs a database connection. The import gates enforce that
// boundary so a caller cannot bypass Organization-scoped storage behavior.
package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey serializes schema migration across concurrently starting instances.
const migrationLockKey int64 = 7_263_041_998_120_001

// ErrUnknownOrganization reports an empty Organization at the storage boundary.
var ErrUnknownOrganization = errors.New("organization names no tenant")

// Database is the one durable PostgreSQL store owned by a deployment.
type Database struct {
	pool            *pgxpool.Pool
	forwardingAudit atomic.Bool
}

// EnableAuditForwarding makes subsequent Audit Event writes enqueue the same semantic event
// in their transaction. It is called once during composition before requests are served.
func (d *Database) EnableAuditForwarding() { d.forwardingAudit.Store(true) }

// OpenDatabase opens the deployment database. The pool dials lazily; reachability is a
// readiness concern so a transient outage does not prevent the process from explaining it.
func OpenDatabase(ctx context.Context, dsn string) (*Database, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("storage: database connection string is required")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN may contain a password, so the parser's cause is deliberately omitted.
		return nil, errors.New("storage: database has an unusable connection string")
	}
	poolConfig.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTrimSQLInSpanName(),
		otelpgx.WithDisableQuerySpanNamePrefix(),
	)
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, errors.New("storage: database could not be opened")
	}
	return &Database{pool: pool}, nil
}

// Pool returns the deployment pool after rejecting an empty Organization. Store methods
// still receive the Organization and include it in every tenant-owned query.
func (d *Database) Pool(organization tenancy.Organization) (*pgxpool.Pool, error) {
	if organization.IsEmpty() {
		return nil, fmt.Errorf("%w: the empty organization names no tenant", ErrUnknownOrganization)
	}
	return d.pool, nil
}

// Ping reports whether the deployment database is reachable.
func (d *Database) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database is unreachable: %w", err)
	}
	return nil
}

// Close releases the deployment pool.
func (d *Database) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Migrate applies every pending embedded migration under one advisory lock.
func (d *Database) Migrate(ctx context.Context) ([]string, error) {
	pending, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return migrateDatabase(ctx, d.pool, pending)
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

// migrateDatabase applies pending migrations inside one transaction holding a
// transaction-scoped advisory lock. Postgres has transactional DDL, so either every
// pending migration and its ledger row commit together or none do — a half-applied schema
// is not reachable. Concurrent instances serialise on the lock and the loser observes the
// winner's ledger rather than racing it.
func migrateDatabase(
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

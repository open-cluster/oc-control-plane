package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/exaring/otelpgx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockKey int64 = 7_263_041_998_120_001

var ErrUnknownOrganization = errors.New("organization names no tenant")

type Database struct {
	pool *pgxpool.Pool
}

func OpenDatabase(ctx context.Context, dsn string) (*Database, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("storage: database connection string is required")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The parser error may contain a password from the DSN.
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

func (d *Database) poolForOrganization(organization uuid.UUID) (*pgxpool.Pool, error) {
	if organization == uuid.Nil {
		return nil, fmt.Errorf("%w: the empty organization names no tenant", ErrUnknownOrganization)
	}
	return d.pool, nil
}

func (d *Database) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database is unreachable: %w", err)
	}
	return nil
}

func (d *Database) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

func (d *Database) Migrate(ctx context.Context) ([]string, error) {
	pending, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return migrateDatabase(ctx, d.pool, pending)
}

type migration struct {
	version    string
	statements string
}

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

func migrateDatabase(
	ctx context.Context, pool *pgxpool.Pool, migrations []migration,
) (applied []string, err error) {
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback(ctx)
		}
	}()

	// Lock before reading the ledger so concurrent instances cannot both observe it as empty.
	if _, err = transaction.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquiring the migration lock: %w", err)
	}

	ledgerExists, err := relationExists(ctx, transaction, "schema_migration")
	if err != nil {
		return nil, err
	}
	present := make(map[string]struct{})
	if ledgerExists {
		present, err = appliedVersions(ctx, transaction)
		if err != nil {
			return nil, err
		}
		known := make(map[string]struct{}, len(migrations))
		for _, candidate := range migrations {
			known[candidate.version] = struct{}{}
		}
		for version := range present {
			if _, ok := known[version]; !ok {
				return nil, incompatibleSchemaError(version)
			}
		}
	}
	if len(present) == 0 {
		hasObjects, objectErr := hasApplicationObjects(ctx, transaction, ledgerExists)
		if objectErr != nil {
			return nil, objectErr
		}
		if hasObjects {
			return nil, incompatibleSchemaError("")
		}
		if !ledgerExists {
			if _, err = transaction.Exec(ctx, `
				CREATE TABLE public.schema_migration
				(
					version    TEXT        NOT NULL PRIMARY KEY,
					applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
				)`); err != nil {
				return nil, fmt.Errorf("creating the migration ledger: %w", err)
			}
		}
	}

	for _, pending := range migrations {
		if _, done := present[pending.version]; done {
			continue
		}
		if _, err = transaction.Exec(ctx, pending.statements); err != nil {
			return nil, fmt.Errorf("applying %s: %w", pending.version, err)
		}
		if _, err = transaction.Exec(ctx,
			`INSERT INTO public.schema_migration (version) VALUES ($1)`, pending.version); err != nil {
			return nil, fmt.Errorf("recording %s: %w", pending.version, err)
		}
		applied = append(applied, pending.version)
	}

	if err = transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return applied, nil
}

func relationExists(ctx context.Context, transaction pgx.Tx, name string) (bool, error) {
	var exists bool
	if err := transaction.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspecting the migration ledger: %w", err)
	}
	return exists, nil
}

func hasApplicationObjects(ctx context.Context, transaction pgx.Tx, ledgerExists bool) (bool, error) {
	var exists bool
	if err := transaction.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class object
			JOIN pg_namespace namespace ON namespace.oid = object.relnamespace
			WHERE namespace.nspname = 'public'
			  AND object.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
			  AND ($1 = false OR object.relname <> 'schema_migration')
		) OR EXISTS (
			SELECT 1
			FROM pg_proc object
			JOIN pg_namespace namespace ON namespace.oid = object.pronamespace
			WHERE namespace.nspname = 'public'
		)`, ledgerExists).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspecting the application schema: %w", err)
	}
	return exists, nil
}

func incompatibleSchemaError(version string) error {
	detail := "unrecognized application objects"
	if version != "" {
		detail = fmt.Sprintf("unrecognized migration %q", version)
	}
	return fmt.Errorf("storage: %s; this pre-release database must be recreated", detail)
}

func appliedVersions(ctx context.Context, transaction pgx.Tx) (map[string]struct{}, error) {
	rows, err := transaction.Query(ctx, `SELECT version FROM public.schema_migration`)
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

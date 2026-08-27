package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// scanned is what both a single-row query and a row set offer. Naming it lets one mapping
// function serve every read of a table, so a column added to one query and forgotten in
// another is a compile error rather than a field that is silently always zero.
type scanned interface {
	Scan(destination ...any) error
}

// executor is what both a pool and a transaction offer, so a guarded statement can run
// either alone or as part of a larger write without a second copy of the SQL.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// isUniqueViolation reports whether an error is the named unique constraint failing. The
// constraint is named rather than the code alone checked, because a table has several and
// reporting the wrong one to an operator sends them to fix something that is not broken.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// isForeignKeyViolation reports whether an error is a foreign key failing. Not named,
// unlike the unique constraints: every foreign key on these tables says the same thing to
// a caller — the row you named is not in the scope you named it from — and distinguishing
// them would hand back which half of a crossed tenant boundary was wrong.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// nullableText renders an empty string as SQL NULL.
func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

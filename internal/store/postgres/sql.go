package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

type scanned interface {
	Scan(destination ...any) error
}

type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func TestCurrentSessionDeletionRollsBackWhenDeploymentAuditFails(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	user, issued, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, digest, "")
	if err != nil {
		t.Fatal(err)
	}
	principal := sessionPrincipal(t, database, digest, user.ID)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE FUNCTION refuse_session_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action = 'session.signed-out' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER refuse_session_audit BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION refuse_session_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = database.DeleteCurrentSession(ctx, principal); err == nil {
		t.Fatal("session deletion committed without audit")
	}
	if _, err = database.SessionByToken(ctx, digest); err != nil {
		t.Fatalf("failed deletion changed session: %v", err)
	}
	if _, err = connection.Exec(ctx, `DROP TRIGGER refuse_session_audit ON audit_event`); err != nil {
		t.Fatal(err)
	}
	if err = database.DeleteCurrentSession(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrUnknown) {
		t.Fatalf("deleted session resolved: %v", err)
	}
	var count int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE org_id IS NULL AND action = 'session.signed-out' AND target_id = $1`, issued.ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deployment audit = %d: %v", count, err)
	}
}

func TestSessionLookupIsReadOnly(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	_, issued, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, digest, "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	version := func() string {
		var found string
		if err := connection.QueryRow(ctx, `SELECT xmin::text FROM session WHERE session_id = $1`, issued.ID).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	before := version()
	for range 3 {
		if _, err := database.SessionByToken(ctx, digest); err != nil {
			t.Fatal(err)
		}
	}
	if version() != before {
		t.Fatal("session lookup created a new row version")
	}
}

func TestSessionLookupRejectsExpiredAndDisabledUsers(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	issued := session.Session{ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, issued, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin",
		"encoded password with sufficient length", issued, digest, "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `UPDATE session SET expires_at=now() WHERE session_id=$1`, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("expired session = %v", err)
	}
	if _, err = connection.Exec(ctx, `UPDATE session SET expires_at=now()+interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `UPDATE app_user SET disabled_at=now() WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrUnknown) {
		t.Fatalf("disabled User session = %v", err)
	}
}

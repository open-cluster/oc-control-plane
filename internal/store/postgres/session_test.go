package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func TestSessionRevocationRollsBackWhenDeploymentAuditFails(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", "encoded password with sufficient length", issued, digest)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authz.NewPrincipal(authz.KindUser, user.ID.String(), "Admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	principal = principal.WithCredential(issued.ID.String())
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE FUNCTION refuse_session_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action = 'session.revoked' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER refuse_session_audit BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION refuse_session_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = database.RevokeSession(ctx, principal, issued.ID); err == nil {
		t.Fatal("revocation committed without audit")
	}
	if _, err = database.SessionByToken(ctx, digest); err != nil {
		t.Fatalf("failed revocation changed session: %v", err)
	}
	if _, err = connection.Exec(ctx, `DROP TRIGGER refuse_session_audit ON audit_event`); err != nil {
		t.Fatal(err)
	}
	if err = database.RevokeSession(ctx, principal, issued.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE org_id IS NULL AND action = 'session.revoked' AND target_id = $1`, issued.ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deployment audit = %d: %v", count, err)
	}
}

func TestSessionLookupOnlyWritesWhenLastSeenIsDue(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	if _, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", "encoded password with sufficient length", issued, digest); err != nil {
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
		t.Fatal("fresh session lookup created a new row version")
	}
	if _, err := connection.Exec(ctx, `UPDATE session SET last_seen_at = now() - interval '2 minutes' WHERE session_id = $1`, issued.ID); err != nil {
		t.Fatal(err)
	}
	due := version()
	found, err := database.SessionByToken(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if version() == due || time.Since(found.Session.LastSeenAt) > time.Minute {
		t.Fatal("due session was not refreshed")
	}
}

package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func TestSessionCleanupIsGlobalAndBounded(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin",
		"encoded password with sufficient length", issued, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err := connection.Exec(ctx, `INSERT INTO operator_session
		(session_id, token_digest, user_id, issued_at, expires_at, revoked_at)
		SELECT md5(n::text)::uuid, decode(md5(n::text) || md5(n::text), 'hex'), $1,
		       now() - interval '3 days',
		       CASE WHEN n <= 1001 THEN now() - interval '2 days' ELSE now() + interval '1 day' END,
		       CASE WHEN n = 1002 THEN now() - interval '2 days' WHEN n = 1003 THEN now() END
		FROM generate_series(1, 1003) n`, user.ID); err != nil {
		t.Fatal(err)
	}
	for pass, want := range []int64{1000, 2, 0} {
		removed, err := database.PruneSessions(ctx)
		if err != nil || removed != want {
			t.Fatalf("pass %d: removed=%d err=%v, want %d", pass, removed, err, want)
		}
	}
	var remaining, audits int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM operator_session`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action = 'local.bootstrap-completed'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 || audits != 1 {
		t.Fatalf("remaining sessions=%d bootstrap audit records=%d", remaining, audits)
	}
}

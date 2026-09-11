package controlplane

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestSessionHousekeepingRunsWithoutAnOrganization(t *testing.T) {
	plane := startIdentityPlane(t)
	created := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap",
		map[string]any{"email": "admin@example.test", "password": "administrator password"}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap: %d: %s", created.status, created.body)
	}
	cookie := sessionCookie(t, created)
	plane.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err := connection.Exec(ctx, `UPDATE session
		SET issued_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	restarted := startIdentityPlane(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.OperatorTokenDigest = nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var sessions, organizations int
		if err := connection.QueryRow(ctx, `SELECT (SELECT count(*) FROM session),
			(SELECT count(*) FROM organization)`).Scan(&sessions, &organizations); err != nil {
			t.Fatal(err)
		}
		if organizations != 0 {
			t.Fatal("fixture unexpectedly has an Organization")
		}
		if sessions == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup housekeeping retained %d expired sessions", sessions)
		}
		time.Sleep(20 * time.Millisecond)
	}
	response := restarted.call(t, http.MethodGet, "http://"+restarted.operator+"/api/v1/session", nil, asSession(cookie))
	if response.status != http.StatusUnauthorized {
		t.Fatalf("expired session returned %d", response.status)
	}
}

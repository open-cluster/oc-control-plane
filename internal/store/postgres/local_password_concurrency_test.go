package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestRecoveryAndLocalSignInSerializeBothLockOrders(t *testing.T) {
	for _, first := range []string{"sign-in", "recovery"} {
		t.Run(first, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			dsn := postgresDSN(t)
			database := openDatabaseForTest(t, dsn)
			if _, err := database.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			previous := "previous encoded password verifier"
			user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", previous, issued, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			principal, _ := authz.NewPrincipal(authz.KindUser, user.ID.String(), "Admin", nil)
			membership, err := database.CreateOrganization(ctx, principal, "Operations")
			if err != nil {
				t.Fatal(err)
			}
			organization := membership.Organization
			connection, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close(context.Background()) }()
			if _, err = connection.Exec(ctx, `SELECT pg_advisory_lock(48151);
				CREATE FUNCTION hold_password_audit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock(48151); RETURN NEW; END $$;
				CREATE TRIGGER hold_password_audit BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION hold_password_audit()`); err != nil {
				t.Fatal(err)
			}
			issued.ID, issued.UserID = uuid.New(), user.ID
			digest := make([]byte, 32)
			digest[0] = 1
			signedIn := make(chan error, 1)
			recovered := make(chan error, 1)
			issue := func() {
				signedIn <- database.IssueLocalSession(ctx, organization, issued, digest, principal.Actor(), audit.Detail{}, previous)
			}
			recoverUser := func() {
				recovered <- database.RecoverLocalPassword(ctx, user.ID, "replacement encoded password verifier")
			}
			if first == "sign-in" {
				go issue()
			} else {
				go recoverUser()
			}
			awaitDatabaseLockWaiters(t, ctx, connection, 1)
			if first == "sign-in" {
				go recoverUser()
			} else {
				go issue()
			}
			awaitDatabaseLockWaiters(t, ctx, connection, 2)
			if _, err := connection.Exec(ctx, `SELECT pg_advisory_unlock(48151)`); err != nil {
				t.Fatal(err)
			}
			if err := <-recovered; err != nil {
				t.Fatal(err)
			}
			if err := <-signedIn; first == "sign-in" && err != nil || first == "recovery" && !errors.Is(err, storage.ErrLocalCredentialUnknown) {
				t.Fatalf("overlapping sign-in = %v", err)
			}
			_, err = database.SessionByToken(ctx, digest)
			if !errors.Is(err, session.ErrRevoked) && !errors.Is(err, session.ErrUnknown) {
				t.Fatalf("old verifier left a live session: %v", err)
			}
		})
	}
}

func awaitDatabaseLockWaiters(t *testing.T, ctx context.Context, connection *pgx.Conn, wanted int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := connection.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= wanted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe %d blocked database transactions", wanted)
}

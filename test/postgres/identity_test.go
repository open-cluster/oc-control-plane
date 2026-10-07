package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestLocalBootstrapRollsBackUserWhenTheSessionCannotBeIssued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	issued := session.Session{
		ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}

	_, _, err := database.BootstrapLocalUser(ctx,
		"Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, nil, "")
	if err == nil {
		t.Fatal("bootstrap with an invalid session digest succeeded")
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	var rows int
	if err = connection.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM organization) + (SELECT count(*) FROM app_user) +
		(SELECT count(*) FROM organization_membership) + (SELECT count(*) FROM local_password) +
		(SELECT count(*) FROM session)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("bootstrap rollback left %d identity rows: %v", rows, err)
	}
	issued.ID = uuid.New()
	user, stored, err := database.BootstrapLocalUser(ctx,
		"Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued,
		make([]byte, 32), "")
	if err != nil {
		t.Fatalf("bootstrap after rolled-back session issuance: %v", err)
	}
	if user.ID == uuid.Nil || stored.ID == uuid.Nil || stored.ID == issued.ID {
		t.Fatalf("database-generated IDs = user:%s session:%s", user.ID, stored.ID)
	}
}

func TestMembershipIsUniqueByUserAndRemovalKeepsTheUser(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin",
		"encoded password with sufficient length", session.Session{
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}, make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	var another uuid.UUID
	if err = connection.QueryRow(ctx, `INSERT INTO organization (display_name,created_by)
		VALUES ('Another',$1) RETURNING org_id`, user.ID.String()).Scan(&another); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `INSERT INTO organization_membership (org_id,user_id,role)
		VALUES ($1,$2,'viewer')`, another, user.ID); err == nil {
		t.Fatal("a second Membership for one User was accepted")
	}
	if _, err = connection.Exec(ctx, `DELETE FROM organization_membership WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	var users int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE user_id=$1`, user.ID).Scan(&users); err != nil || users != 1 {
		t.Fatalf("durable User after Membership removal = %d: %v", users, err)
	}
}

func TestAUserWithLocalCredentialsCannotBeDeletedByCascade(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err := connection.Exec(ctx, `DELETE FROM app_user WHERE user_id = $1`, user.ID); err == nil {
		t.Fatal("deleting a User erased its independently managed local credential")
	}
	if _, err := connection.Exec(ctx, `UPDATE app_user SET disabled_at=now() WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	issued.ID = uuid.New()
	_, _, err = database.BootstrapLocalUser(ctx, "Another", "another@example.test", "Another", "encoded password with sufficient length", issued, make([]byte, 32), "")
	if !errors.Is(err, storage.ErrLocalBootstrapComplete) {
		t.Fatalf("bootstrap reopened for a disabled User: %v", err)
	}
}

func TestConcurrentLocalBootstrapCreatesOneUser(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	start := make(chan struct{})
	for index := range 2 {
		go func() {
			<-start
			issued := session.Session{
				ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			_, _, err := database.BootstrapLocalUser(ctx,
				"Operations", fmt.Sprintf("admin-%d@example.test", index), "Admin",
				"encoded password with sufficient length", issued, make([]byte, 32), "")
			results <- err
		}()
	}
	close(start)

	var succeeded, complete int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, storage.ErrLocalBootstrapComplete):
			complete++
		default:
			t.Fatalf("bootstrap failed unexpectedly: %v", err)
		}
	}
	if succeeded != 1 || complete != 1 {
		t.Fatalf("successes=%d completed=%d, want one each", succeeded, complete)
	}
}

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
			issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			previous := "previous encoded password verifier"
			user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", previous, issued, make([]byte, 32), "")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := database.LocalIdentityByEmail(ctx, "admin@example.test")
			if err != nil {
				t.Fatal(err)
			}
			organization := identity.Membership.Organization
			principal, err := authz.NewPrincipal(user.ID, issued.ID, "Admin", "admin@example.test", identity.Membership)
			if err != nil {
				t.Fatal(err)
			}
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
				_, err := database.IssueLocalSession(ctx, organization, issued, digest, principal.Actor(), "", audit.Detail{}, previous)
				signedIn <- err
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
			if !errors.Is(err, session.ErrUnknown) {
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

func TestLocalSessionIssuanceRejectsReplacedVerifier(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	previous := "previous encoded password verifier"
	user, issued, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", previous, issued, make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	principal := sessionPrincipal(t, database, make([]byte, 32), user.ID)
	signedIn, err := database.SessionByToken(ctx, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	organization := signedIn.Membership.Organization
	if err := database.ChangeLocalPassword(ctx, principal, previous, "replacement encoded password verifier"); err != nil {
		t.Fatal(err)
	}
	issued.ID, issued.UserID = uuid.New(), user.ID
	digest := make([]byte, 32)
	digest[0] = 1
	_, err = database.IssueLocalSession(ctx, organization, issued, digest, principal.Actor(), "", audit.Detail{}, previous)
	if !errors.Is(err, storage.ErrLocalCredentialUnknown) {
		t.Fatalf("issuing with replaced verifier = %v", err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrUnknown) {
		t.Fatalf("stale sign-in created session: %v", err)
	}
}

func TestPasswordMutationsRollBackWhenAuditFails(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "self-service"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := postgresDSN(t)
			database := openDatabaseForTest(t, dsn)
			if _, err := database.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			digest := make([]byte, 32)
			previous := "previous encoded password verifier"
			user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", previous, issued, digest, "")
			if err != nil {
				t.Fatal(err)
			}
			principal := sessionPrincipal(t, database, digest, user.ID)
			connection, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close(ctx) }()
			if _, err = connection.Exec(ctx, `CREATE FUNCTION refuse_password_audit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF NEW.action IN ('local.password-changed', 'local.password-recovered') THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER refuse_password_audit BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION refuse_password_audit()`); err != nil {
				t.Fatal(err)
			}
			if recovery {
				err = database.RecoverLocalPassword(ctx, user.ID, "replacement encoded password verifier")
			} else {
				err = database.ChangeLocalPassword(ctx, principal, previous, "replacement encoded password verifier")
			}
			if err == nil {
				t.Fatal("password mutation committed without audit")
			}
			if got, err := database.LocalPasswordHash(ctx, principal); err != nil || got != previous {
				t.Fatalf("password was not rolled back: %v", err)
			}
			if _, err := database.SessionByToken(ctx, digest); err != nil {
				t.Fatalf("session revocation was not rolled back: %v", err)
			}
		})
	}
}

func TestRecoveryDoesNotConvertOIDCUsers(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	user := uuid.New()
	if _, err = connection.Exec(ctx, `INSERT INTO app_user (user_id, issuer, subject, email) VALUES ($1, 'https://issuer.example', 'subject', 'oidc@example.test')`, user); err != nil {
		t.Fatal(err)
	}
	if err = database.RecoverLocalPassword(ctx, user, "replacement encoded password verifier"); !errors.Is(err, storage.ErrLocalCredentialUnknown) {
		t.Fatalf("OIDC recovery = %v", err)
	}
	var count int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM local_password WHERE user_id = $1`, user).Scan(&count); err != nil || count != 0 {
		t.Fatalf("OIDC User acquired a local credential: %d: %v", count, err)
	}
}

func TestRecoveryTargetsExistingLocalUserAndDeletesSessions(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", "previous encoded password verifier", issued, digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = database.RecoverLocalPassword(ctx, uuid.New(), "replacement encoded password verifier"); !errors.Is(err, storage.ErrLocalCredentialUnknown) {
		t.Fatalf("unknown recovery = %v", err)
	}
	if _, err = database.SessionByToken(ctx, digest); err != nil {
		t.Fatalf("unknown recovery affected session: %v", err)
	}
	principal := sessionPrincipal(t, database, digest, user.ID)
	if err = database.RecoverLocalPassword(ctx, user.ID, "replacement encoded password verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrUnknown) {
		t.Fatalf("recovery retained session: %v", err)
	}
	if got, err := database.LocalPasswordHash(ctx, principal); err != nil || got != "replacement encoded password verifier" {
		t.Fatalf("recovered verifier = %q: %v", got, err)
	}
}

func TestSessionCleanupIsGlobalAndBounded(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin",
		"encoded password with sufficient length", issued, make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err := connection.Exec(ctx, `INSERT INTO session
		(session_id, credential_digest, user_id, expires_at)
		SELECT md5(n::text)::uuid, decode(md5(n::text) || md5(n::text), 'hex'), $1,
		       CASE WHEN n <= 1001 THEN now() - interval '2 days' ELSE now() + interval '1 day' END
		FROM generate_series(1, 1003) n`, user.ID); err != nil {
		t.Fatal(err)
	}
	for pass, want := range []int64{1000, 1, 0} {
		removed, err := database.PruneSessions(ctx)
		if err != nil || removed != want {
			t.Fatalf("pass %d: removed=%d err=%v, want %d", pass, removed, err, want)
		}
	}
	var remaining, audits int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM session`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action = 'local.bootstrap-completed'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if remaining != 3 || audits != 1 {
		t.Fatalf("remaining sessions=%d bootstrap audit records=%d", remaining, audits)
	}
}

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
	if _, err = connection.Exec(ctx, `UPDATE session SET expires_at=$2 WHERE session_id=$1`,
		issued.ID, time.Now().UTC().Add(-time.Hour)); err != nil {
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

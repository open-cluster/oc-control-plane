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

func TestLocalSessionIssuanceRejectsReplacedVerifier(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	previous := "previous encoded password verifier"
	user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", previous, issued, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authz.NewPrincipal(authz.KindUser, user.ID.String(), "Admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	membership, err := database.CreateOrganization(ctx, principal, "Operations")
	if err != nil {
		t.Fatal(err)
	}
	organization := membership.Organization
	if err := database.ChangeLocalPassword(ctx, principal, previous, "replacement encoded password verifier"); err != nil {
		t.Fatal(err)
	}
	issued.ID, issued.UserID = uuid.New(), user.ID
	digest := make([]byte, 32)
	digest[0] = 1
	err = database.IssueLocalSession(ctx, organization, issued, digest, principal.Actor(), audit.Detail{}, previous)
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
			issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			digest := make([]byte, 32)
			previous := "previous encoded password verifier"
			user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", previous, issued, digest)
			if err != nil {
				t.Fatal(err)
			}
			principal, _ := authz.NewPrincipal(authz.KindUser, user.ID.String(), "Admin", nil)
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

func TestRecoveryTargetsExistingLocalUserAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	digest := make([]byte, 32)
	user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", "previous encoded password verifier", issued, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.RecoverLocalPassword(ctx, uuid.New(), "replacement encoded password verifier"); !errors.Is(err, storage.ErrLocalCredentialUnknown) {
		t.Fatalf("unknown recovery = %v", err)
	}
	if _, err = database.SessionByToken(ctx, digest); err != nil {
		t.Fatalf("unknown recovery affected session: %v", err)
	}
	if err = database.RecoverLocalPassword(ctx, user.ID, "replacement encoded password verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SessionByToken(ctx, digest); !errors.Is(err, session.ErrRevoked) {
		t.Fatalf("recovery retained session: %v", err)
	}
	principal, _ := authz.NewPrincipal(authz.KindUser, user.ID.String(), "Admin", nil)
	if got, err := database.LocalPasswordHash(ctx, principal); err != nil || got != "replacement encoded password verifier" {
		t.Fatalf("recovered verifier = %q: %v", got, err)
	}
}

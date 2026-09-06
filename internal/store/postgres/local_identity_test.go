package storage_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestLocalBootstrapRollsBackUserWhenTheSessionCannotBeIssued(t *testing.T) {
	t.Parallel()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	issued := session.Session{
		ID:       uuid.New(),
		IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}

	_, err := database.BootstrapLocalUser(context.Background(),
		"admin@example.test", "Admin", "encoded password with sufficient length", issued, nil)
	if err == nil {
		t.Fatal("bootstrap with an invalid session digest succeeded")
	}
	issued.ID = uuid.New()
	if _, err = database.BootstrapLocalUser(context.Background(),
		"admin@example.test", "Admin", "encoded password with sufficient length", issued,
		make([]byte, 32)); err != nil {
		t.Fatalf("bootstrap after rolled-back session issuance: %v", err)
	}
}

func TestRetainedInstallationRetiresBootstrapWithoutReplacingCredentials(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	baseline, err := os.ReadFile("migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()); INSERT INTO schema_migration(version) VALUES ('0001_baseline')`); err != nil {
		t.Fatal(err)
	}
	user := uuid.New()
	const previous = "retained encoded password verifier"
	if _, err = connection.Exec(ctx, `INSERT INTO app_user(user_id,issuer,subject,email) VALUES ($1,'opencluster:local','retained@example.test','retained@example.test')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `INSERT INTO local_password(user_id,password_hash) VALUES ($1,$2)`, user, previous); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	if _, err = database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var verifier string
	if err = connection.QueryRow(ctx, `SELECT password_hash FROM local_password WHERE user_id=$1`, user).Scan(&verifier); err != nil || verifier != previous {
		t.Fatalf("upgrade replaced credential: %v", err)
	}
	if _, err = connection.Exec(ctx, `DELETE FROM app_user WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if _, err = database.BootstrapLocalUser(ctx, "new@example.test", "New", previous, issued, make([]byte, 32)); !errors.Is(err, storage.ErrLocalBootstrapComplete) {
		t.Fatalf("retained bootstrap reopened: %v", err)
	}
}

func TestBootstrapRemainsRetiredAfterItsUserIsDeleted(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, err := database.BootstrapLocalUser(ctx, "admin@example.test", "Admin", "encoded password with sufficient length", issued, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err := connection.Exec(ctx, `DELETE FROM app_user WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	issued.ID = uuid.New()
	_, err = database.BootstrapLocalUser(ctx, "another@example.test", "Another", "encoded password with sufficient length", issued, make([]byte, 32))
	if !errors.Is(err, storage.ErrLocalBootstrapComplete) {
		t.Fatalf("bootstrap reopened after User deletion: %v", err)
	}
}

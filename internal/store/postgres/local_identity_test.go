package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
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
		ID:       uuid.New(),
		IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}

	_, _, err := database.BootstrapLocalUser(ctx,
		"Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, nil)
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
		make([]byte, 32))
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
			IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		}, make([]byte, 32))
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
	issued := session.Session{ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	user, _, err := database.BootstrapLocalUser(ctx, "Operations", "admin@example.test", "Admin", "encoded password with sufficient length", issued, make([]byte, 32))
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
	_, _, err = database.BootstrapLocalUser(ctx, "Another", "another@example.test", "Another", "encoded password with sufficient length", issued, make([]byte, 32))
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
				ID: uuid.New(), IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			_, _, err := database.BootstrapLocalUser(ctx,
				"Operations", fmt.Sprintf("admin-%d@example.test", index), "Admin",
				"encoded password with sufficient length", issued, make([]byte, 32))
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

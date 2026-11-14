package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestLocalSignupCannotReopenAfterAccountRemoval(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	user, _, err := database.BootstrapLocalUser(ctx, "Platform Team", "admin@example.test", "Admin",
		"encoded password with sufficient length", make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	for _, table := range []string{"session", "local_password", "organization_membership", "app_user"} {
		if _, err = connection.Exec(ctx, "DELETE FROM "+table+" WHERE user_id = $1", user.ID); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	database = openDatabaseForTest(t, dsn)
	if _, err = database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, err = database.BootstrapLocalUser(ctx, "Another Team", "another@example.test", "Another",
		"encoded password with sufficient length", make([]byte, 32), "")
	if !errors.Is(err, storage.ErrLocalBootstrapComplete) {
		t.Fatalf("signup reopened after account removal and restart: %v", err)
	}
}

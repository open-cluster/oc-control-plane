package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
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

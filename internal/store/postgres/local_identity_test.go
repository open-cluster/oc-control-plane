package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func TestLocalBootstrapRollsBackIdentityWhenTheSessionCannotBeIssued(t *testing.T) {
	t.Parallel()
	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org := organization(t, "atomic-bootstrap")
	issued := session.Session{
		ID: uuid.New(), Organization: org.String(),
		IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}

	_, _, err := database.BootstrapLocalAdmin(context.Background(), org,
		"admin@example.test", "Admin", "encoded password", issued, nil, audit.Detail{})
	if err == nil {
		t.Fatal("bootstrap with an invalid session digest succeeded")
	}
	exists, existsErr := database.OrganizationExists(context.Background(), org)
	if existsErr != nil {
		t.Fatalf("OrganizationExists: %v", existsErr)
	}
	if exists {
		t.Fatal("failed session issuance left the bootstrapped Organization committed")
	}
}

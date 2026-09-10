package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("controlplane"),
		tcpostgres.WithUsername("controlplane"),
		tcpostgres.WithPassword("controlplane"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		noContainerRuntime(t, err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func openDatabaseForTest(t *testing.T, dsn string) *storage.Database {
	t.Helper()
	opened, err := storage.OpenDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(opened.Close)
	return opened
}

func organization(t *testing.T, id string) tenancy.Organization {
	t.Helper()
	if _, err := uuid.Parse(id); err != nil {
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte(id)).String()
	}
	value, err := tenancy.NewOrganization(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	return value
}

func ownerOf(t *testing.T, organization tenancy.Organization) authz.Principal {
	t.Helper()
	return memberOf(t, organization, authz.Admin)
}

func memberOf(
	t *testing.T, organization tenancy.Organization, role authz.Role,
) authz.Principal {
	t.Helper()

	principal, err := authz.NewPrincipal(authz.KindUser, "user-under-test", "Test Operator",
		[]authz.Membership{{Organization: organization, Role: role}})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

func aStranger(t *testing.T) authz.Principal {
	t.Helper()
	return memberOf(t, organization(t, "somebody else"), authz.Admin)
}

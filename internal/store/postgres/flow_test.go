package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestOIDCFlowIsDisposable(t *testing.T) {
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
	org := organization(t, "oidc-flow")
	ensureOrganization(t, connection, org)

	wanted := storage.DeploymentSignInFlow{
		Organization: org.String(), CodeVerifier: "verifier", Nonce: "nonce",
		ReturnTo: "/incidents", ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := database.StartDeploymentSignIn(ctx, org, wanted, "live-state"); err != nil {
		t.Fatal(err)
	}
	got, err := database.RedeemDeploymentSignIn(ctx, "live-state")
	if err != nil || got.Organization != wanted.Organization || got.ReturnTo != wanted.ReturnTo {
		t.Fatalf("redeemed flow = %+v, error = %v", got, err)
	}
	if _, err := database.RedeemDeploymentSignIn(ctx, "live-state"); !errors.Is(err, storage.ErrFlowUnknown) {
		t.Fatalf("replay = %v", err)
	}
	if err := database.StartDeploymentSignIn(ctx, org, storage.DeploymentSignInFlow{
		Organization: org.String(), ReturnTo: "/", ExpiresAt: time.Now().Add(-time.Minute),
	}, "expired-state"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"expired-state", "unknown-state"} {
		if _, err := database.RedeemDeploymentSignIn(ctx, state); !errors.Is(err, storage.ErrFlowUnknown) {
			t.Errorf("%s = %v", state, err)
		}
	}
}

func TestIntegrationConnectFlowIsDisposable(t *testing.T) {
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
	org := organization(t, "connect-flow")
	ensureOrganization(t, connection, org)

	wanted := integrations.ConnectFlow{
		Organization: org.String(), Provider: "github", Principal: "user-1",
		ReturnTo: "/integrations", ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := database.StartConnectFlow(ctx, org, wanted, "live-state"); err != nil {
		t.Fatal(err)
	}
	got, err := database.RedeemConnectFlow(ctx, "live-state")
	if err != nil || got.Provider != wanted.Provider || got.Principal != wanted.Principal {
		t.Fatalf("redeemed flow = %+v, error = %v", got, err)
	}
	if _, err := database.RedeemConnectFlow(ctx, "live-state"); !errors.Is(err, integrations.ErrConnectFlowUnknown) {
		t.Fatalf("replay = %v", err)
	}
	if err := database.StartConnectFlow(ctx, org, integrations.ConnectFlow{
		Organization: org.String(), Provider: "github", Principal: "user-1",
		ReturnTo: "/", ExpiresAt: time.Now().Add(-time.Minute),
	}, "expired-state"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"expired-state", "unknown-state"} {
		if _, err := database.RedeemConnectFlow(ctx, state); !errors.Is(err, integrations.ErrConnectFlowUnknown) {
			t.Errorf("%s = %v", state, err)
		}
	}
}

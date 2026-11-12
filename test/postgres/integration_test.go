package storage_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestCredentialDestinationCannotChangeWithoutReplacement(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	principal := ownerOf(t, organization)
	integration, err := database.CreateIntegration(ctx, principal, organization, integrations.NewIntegration{
		Provider: "prometheus", Name: "Metrics",
		Configuration:    map[string]any{"url": "https://original.example"},
		CredentialSealed: []byte("sealed credential"),
		Verification:     &integrations.Verification{Status: integrations.StatusVerified, Grants: []string{"prometheus:query"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := "Renamed metrics"
	if _, err := database.UpdateIntegration(ctx, principal, organization, integration.ID, integrations.IntegrationUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateIntegration(ctx, principal, organization, integration.ID, integrations.IntegrationUpdate{
		Name:          &name,
		Configuration: integration.Configuration,
	}); err != nil {
		t.Fatal(err)
	}
	events, err := database.AuditEvents(ctx, principal, organization, audit.Page{})
	if err != nil {
		t.Fatal(err)
	}
	var nameChanges, unchanged int
	for _, event := range events.Events {
		if event.Action != audit.ActionIntegrationRevised || event.Target.ID != integration.ID.String() {
			continue
		}
		if event.Detail["configurationChanged"] != false {
			t.Fatalf("unchanged configuration audit = %v", event.Detail)
		}
		switch event.Detail["nameChanged"] {
		case true:
			nameChanges++
		case false:
			unchanged++
		}
	}
	if nameChanges != 1 || unchanged != 1 {
		t.Fatalf("update audit reported %d name changes and %d unchanged updates, want one each", nameChanges, unchanged)
	}
	_, err = database.UpdateIntegration(ctx, principal, organization, integration.ID, integrations.IntegrationUpdate{
		Configuration: map[string]any{"url": "https://different.example"},
	})
	if err == nil {
		t.Fatal("changed destination retained an existing credential")
	}
	current, err := database.Integration(ctx, organization, integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != name || current.Configuration["url"] != "https://original.example" || !bytes.Equal(current.CredentialSealed, integration.CredentialSealed) || current.Status != integrations.StatusVerified {
		t.Fatalf("rejected update changed integration: %+v", current)
	}
}

func TestStaleCredentialVerificationCannotOverwriteChangedAccess(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	principal := ownerOf(t, organization)
	integration, err := database.CreateIntegration(ctx, principal, organization, integrations.NewIntegration{
		Provider: "prometheus", Name: "Metrics", Configuration: map[string]any{"url": "https://original.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	verification := integrations.Verification{Status: integrations.StatusVerified}
	current, err := database.UpdateIntegration(ctx, principal, organization, integration.ID, integrations.IntegrationUpdate{
		Configuration: map[string]any{"url": "https://changed.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.Disconnected {
		t.Fatal("stale verification must be rejected while the integration is active")
	}
	if _, err := database.RecordIntegrationVerification(ctx, principal, organization, integration, verification); !errors.Is(err, integrations.ErrConflict) {
		t.Fatalf("stale verification = %v, want conflict", err)
	}
	if _, err := database.ReplaceIntegrationCredential(ctx, principal, organization, integration,
		integrations.IntegrationUpdate{}, []byte("replacement"), verification, nil); !errors.Is(err, integrations.ErrConflict) {
		t.Fatalf("stale credential replacement = %v, want conflict", err)
	}
	if err := database.DisconnectIntegration(ctx, principal, organization, integration.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordIntegrationVerification(ctx, principal, organization, current, verification); !errors.Is(err, integrations.ErrDisconnected) {
		t.Fatalf("disconnected verification = %v, want disconnected", err)
	}
	if _, err := database.ReplaceIntegrationCredential(ctx, principal, organization, current,
		integrations.IntegrationUpdate{}, []byte("replacement"), verification, nil); !errors.Is(err, integrations.ErrDisconnected) {
		t.Fatalf("disconnected replacement = %v, want disconnected", err)
	}
}

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
	wanted := storage.DeploymentSignInFlow{
		CodeVerifier: "verifier", Nonce: "nonce",
		ReturnTo: "/incidents", ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := database.StartDeploymentSignIn(ctx, wanted, "live-state"); err != nil {
		t.Fatal(err)
	}
	got, err := database.RedeemDeploymentSignIn(ctx, "live-state")
	if err != nil || got.ReturnTo != wanted.ReturnTo {
		t.Fatalf("redeemed flow = %+v, error = %v", got, err)
	}
	if _, err := database.RedeemDeploymentSignIn(ctx, "live-state"); !errors.Is(err, storage.ErrFlowUnknown) {
		t.Fatalf("replay = %v", err)
	}
	if err := database.StartDeploymentSignIn(ctx, storage.DeploymentSignInFlow{
		ReturnTo: "/", ExpiresAt: time.Now().Add(-time.Minute),
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

func slackInstallation(workspace string) *integrations.Installation {
	return &integrations.Installation{
		Key:             integrations.InstallationKey{"A0OPENCLUSTER", workspace},
		ProviderActorID: "U0BOT",
	}
}

func connectSlack(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	name string, installed *integrations.Installation,
) (integrations.Integration, error) {
	t.Helper()

	return database.CreateIntegration(context.Background(), ownerOf(t, organization),
		organization, integrations.NewIntegration{
			Provider:      "slack",
			Name:          name,
			Configuration: map[string]any{"teamId": installed.Key[len(installed.Key)-1]},
			Installation:  installed,
		})
}

func TestIntegrationListingAppliesAscendingSortAcrossPages(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	first, err := connectSlack(t, database, organization, "first", slackInstallation("T0FIRST"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := connectSlack(t, database, organization, "second", slackInstallation("T0SECOND"))
	if err != nil {
		t.Fatal(err)
	}

	page, err := database.QueryIntegrations(context.Background(), ownerOf(t, organization),
		organization, integrations.Query{Page: integrations.Page{Limit: 1}, Sort: "createdAt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Integrations) != 1 || page.Integrations[0].ID != first.ID || page.Next == "" {
		t.Fatalf("first page = %+v", page)
	}
	page, err = database.QueryIntegrations(context.Background(), ownerOf(t, organization),
		organization, integrations.Query{
			Page: integrations.Page{Limit: 1, After: page.Next}, Sort: "createdAt",
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Integrations) != 1 || page.Integrations[0].ID != second.ID {
		t.Fatalf("second page = %+v", page)
	}
}

func TestAConnectedWorkspaceResolvesToItsIntegrationAndTenant(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	installed := slackInstallation("T0ACME")
	created, err := connectSlack(t, database, organization, "Slack — Acme", installed)
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	found, routing, err := database.IntegrationByInstallation(context.Background(),
		"slack", installed.Key)
	if err != nil {
		t.Fatalf("resolving the installation: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("resolved integration %s, want %s", found.ID, created.ID)
	}
	if found.OrgID != organization.String() {
		t.Errorf("resolved organization %q, want %q", found.OrgID, organization)
	}
	if routing.ProviderActorID != installed.ProviderActorID {
		t.Errorf("resolved provider actor %q, want %q",
			routing.ProviderActorID, installed.ProviderActorID)
	}
}

func TestAWorkspaceNobodyInstalledResolvesToNothing(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	if _, err := connectSlack(t, database, organization, "Slack — Acme",
		slackInstallation("T0ACME")); err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	for name, key := range map[string]integrations.InstallationKey{
		"another workspace": {"A0OPENCLUSTER", "T0STRANGER"},
		"another app":       {"A0SOMETHINGELSE", "T0ACME"},
		"no workspace":      {"A0OPENCLUSTER"},
		"nothing at all":    {},
	} {
		_, _, err := database.IntegrationByInstallation(context.Background(),
			"slack", key)
		if !errors.Is(err, integrations.ErrUnknown) {
			t.Errorf("%s resolved to %v, want unknown", name, err)
		}
	}
}

func TestOneWorkspaceCannotBeClaimedTwice(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	if _, err := connectSlack(t, database, organization, "Slack — first",
		slackInstallation("T0ACME")); err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	_, err := connectSlack(t, database, organization, "Slack — second",
		slackInstallation("T0ACME"))
	if !errors.Is(err, integrations.ErrInstallationTaken) {
		t.Fatalf("a second claim on one installation = %v, want ErrInstallationTaken", err)
	}

	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integration WHERE org_id = $1 AND provider = $2`,
		organization, "slack").Scan(&count); err != nil {
		t.Fatalf("counting integrations: %v", err)
	}
	if count != 1 {
		t.Errorf("%d slack integrations exist after a refused second claim, want 1", count)
	}
}

func TestAnIntegrationWithNoInstallationRoutesNothing(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	created, err := database.CreateIntegration(context.Background(),
		ownerOf(t, organization), organization, integrations.NewIntegration{
			Provider: "slack",
			Name:     "Slack — pasted",
		})
	if err != nil {
		t.Fatalf("creating a pasted-token slack integration: %v", err)
	}

	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integration_installation WHERE integration_id = $1`,
		created.ID).Scan(&rows); err != nil {
		t.Fatalf("counting installations: %v", err)
	}
	if rows != 0 {
		t.Errorf("a pasted-token integration recorded %d routing rows, want none", rows)
	}
}

func TestAnInstallationCannotNameNothing(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	_, err := database.CreateIntegration(context.Background(), ownerOf(t, organization),
		organization, integrations.NewIntegration{
			Provider:     "slack",
			Name:         "Slack — nowhere",
			Installation: &integrations.Installation{ProviderActorID: "U0BOT"},
		})
	if !errors.Is(err, integrations.ErrInvalidInstallation) {
		t.Fatalf("an installation naming nothing = %v, want ErrInvalidInstallation", err)
	}
}

func TestDisconnectingTakesTheRoutingRecordWithIt(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	installed := slackInstallation("T0ACME")
	created, err := database.CreateIntegration(context.Background(), ownerOf(t, organization), organization, integrations.NewIntegration{
		Provider: "slack", Name: "Slack — Acme", Installation: installed,
		CredentialSealed: []byte("sealed credential"), WebhookSecretDigest: make([]byte, 32),
		Verification: &integrations.Verification{Status: integrations.StatusVerified, Grants: []string{"slack:history"}},
	})
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}
	if err := database.DisconnectIntegration(context.Background(), ownerOf(t, organization),
		organization, created.ID); err != nil {
		t.Fatalf("disconnecting: %v", err)
	}

	_, _, err = database.IntegrationByInstallation(context.Background(),
		"slack", installed.Key)
	if !errors.Is(err, integrations.ErrUnknown) {
		t.Errorf("a disconnected workspace still resolves: %v", err)
	}
	retained, err := database.Integration(context.Background(), organization, created.ID)
	if err != nil || !retained.Disconnected || len(retained.CredentialSealed) != 0 || len(retained.WebhookSecretDigest) != 0 || retained.Status != "" || !retained.VerifiedAt.IsZero() || len(retained.VerificationGrants) != 0 || retained.Installation != nil {
		t.Fatalf("disconnect retained active access: %+v %v", retained, err)
	}

	if err := database.DisconnectIntegration(context.Background(), ownerOf(t, organization), organization, created.ID); err != nil {
		t.Fatalf("repeated disconnect: %v", err)
	}
	events, err := database.AuditEvents(context.Background(), ownerOf(t, organization), organization, audit.Page{})
	if err != nil {
		t.Fatal(err)
	}
	var transitions, repeats int
	for _, event := range events.Events {
		if event.Action != audit.ActionIntegrationDeleted || event.Target.ID != created.ID.String() {
			continue
		}
		if event.Detail["disconnected"] == true {
			transitions++
		}
		if event.Detail["alreadyDisconnected"] == true {
			repeats++
		}
	}
	if transitions != 1 || repeats != 1 {
		t.Fatalf("disconnect audit transitions=%d repeats=%d, want one each", transitions, repeats)
	}
	if reconnected, err := connectSlack(t, database, organization, "Slack — again",
		slackInstallation("T0ACME")); err != nil || reconnected.ID == created.ID {
		t.Errorf("reconnecting a disconnected workspace: %v", err)
	}
}

func TestAnotherTenantCannotTakeAConnectedWorkspace(t *testing.T) {
	t.Parallel()

	database, first, second := twoOrganizationsInOneDatabase(t)

	installed := slackInstallation("T0SHARED-" + uuid.NewString()[:8])
	if _, err := connectSlack(t, database, first, "Slack — first", installed); err != nil {
		t.Fatalf("connecting slack in the first tenant: %v", err)
	}
	_, err := connectSlack(t, database, second, "Slack — second",
		slackInstallation(installed.Key[len(installed.Key)-1]))
	if !errors.Is(err, integrations.ErrInstallationTaken) {
		t.Fatalf("a neighbour claiming the same installation = %v, want ErrInstallationTaken", err)
	}
}

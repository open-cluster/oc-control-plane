package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE ROUTING RECORD, AT THE SEAM THAT DECIDES WHOSE EVENT AN INBOUND MESSAGE IS.
//
// Two properties matter here and nothing else does. An Integration that exists is one an
// event can reach, because the two rows are written together. And one workspace resolves to
// exactly one Integration ACROSS THE DEPLOYMENT — not per tenant — because the first hop of
// installation to integration to organization is what everything after it trusts.

func slackInstallation(workspace string) *integrations.Installation {
	return &integrations.Installation{
		Application: "A0OPENCLUSTER",
		Workspace:   workspace,
		Agent:       "U0BOT",
		Authorizer:  "U0ADMIN",
		Grants:      []string{"chat:write", "app_mentions:read"},
	}
}

func connectSlack(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	name string, installed *integrations.Installation,
) (integrations.Integration, error) {
	t.Helper()

	return placements.CreateIntegration(context.Background(), ownerOf(t, organization),
		organization, integrations.NewIntegration{
			Type:          integrations.TypeSlack,
			Name:          name,
			Configuration: map[string]any{"teamId": installed.Workspace},
			Installation:  installed,
		})
}

func TestAConnectedWorkspaceResolvesToItsIntegrationAndTenant(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	installed := slackInstallation("T0ACME")
	created, err := connectSlack(t, placements, organization, "Slack — Acme", installed)
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	found, routing, err := placements.IntegrationByInstallation(context.Background(),
		integrations.TypeSlack, installed.Key())
	if err != nil {
		t.Fatalf("resolving the installation: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("resolved integration %s, want %s", found.ID, created.ID)
	}
	if found.OrgID != organization.String() {
		t.Errorf("resolved organization %q, want %q", found.OrgID, organization.String())
	}
	// The bot's own identity comes back with it. It is what stops the agent answering its
	// own message, so a resolution that did not carry it would be one the endpoint cannot
	// act on.
	if routing.Agent != installed.Agent {
		t.Errorf("resolved agent %q, want %q", routing.Agent, installed.Agent)
	}
	if len(routing.Grants) != len(installed.Grants) {
		t.Errorf("resolved grants %v, want %v", routing.Grants, installed.Grants)
	}
}

func TestAWorkspaceNobodyInstalledResolvesToNothing(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	if _, err := connectSlack(t, placements, organization, "Slack — Acme",
		slackInstallation("T0ACME")); err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	// A workspace this deployment has never seen, and a key that is not a key. Both answer
	// the same: unknown. An event resolving through a partial key would be an event
	// resolved through a wildcard.
	for name, key := range map[string]integrations.InstallationKey{
		"another workspace": {Application: "A0OPENCLUSTER", Workspace: "T0STRANGER"},
		"another app":       {Application: "A0SOMETHINGELSE", Workspace: "T0ACME"},
		"no workspace":      {Application: "A0OPENCLUSTER"},
		"nothing at all":    {},
	} {
		_, _, err := placements.IntegrationByInstallation(context.Background(),
			integrations.TypeSlack, key)
		if !errors.Is(err, integrations.ErrUnknown) {
			t.Errorf("%s resolved to %v, want unknown", name, err)
		}
	}
}

func TestOneWorkspaceCannotBeClaimedTwice(t *testing.T) {
	t.Parallel()

	// The constraint that has to exist BEFORE any inbound event is accepted. While Slack
	// is outbound-only, two organizations each holding this workspace is harmless — each
	// reads it with its own token and sees only what its own token can see. It stops being
	// harmless the instant an event arrives, because resolution would have two answers at
	// exactly the moment the product starts trusting it has one.
	placements, organization := migratedPlacement(t)
	if _, err := connectSlack(t, placements, organization, "Slack — first",
		slackInstallation("T0ACME")); err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	_, err := connectSlack(t, placements, organization, "Slack — second",
		slackInstallation("T0ACME"))
	if !errors.Is(err, integrations.ErrWorkspaceTaken) {
		t.Fatalf("a second claim on one workspace = %v, want ErrWorkspaceTaken", err)
	}

	// And nothing was left behind. The refusal has to take the Integration with it, or the
	// customer holds a connected integration whose events reach the first one.
	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integration WHERE org_id = $1 AND integration_type_id = $2`,
		organization.String(), int16(integrations.TypeSlack)).Scan(&count); err != nil {
		t.Fatalf("counting integrations: %v", err)
	}
	if count != 1 {
		t.Errorf("%d slack integrations exist after a refused second claim, want 1", count)
	}
}

func TestAnIntegrationWithNoInstallationRoutesNothing(t *testing.T) {
	t.Parallel()

	// The pasted-token path: a credential for READING, which names no installation for a
	// vendor to deliver events to. It is a supported way to connect and it is not an agent
	// installation, and nothing about it should look like one.
	placements, organization := migratedPlacement(t)
	created, err := placements.CreateIntegration(context.Background(),
		ownerOf(t, organization), organization, integrations.NewIntegration{
			Type: integrations.TypeSlack,
			Name: "Slack — pasted",
		})
	if err != nil {
		t.Fatalf("creating a pasted-token slack integration: %v", err)
	}

	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM slack_installation WHERE integration_id = $1`,
		created.ID).Scan(&rows); err != nil {
		t.Fatalf("counting installations: %v", err)
	}
	if rows != 0 {
		t.Errorf("a pasted-token integration recorded %d routing rows, want none", rows)
	}
}

func TestAnInstallationCannotNameNothing(t *testing.T) {
	t.Parallel()

	// A provider returning a key that routes nothing is a programming error, and it is
	// refused loudly rather than written. The alternative is discovering it at the first
	// inbound event, as silence.
	placements, organization := migratedPlacement(t)
	_, err := placements.CreateIntegration(context.Background(), ownerOf(t, organization),
		organization, integrations.NewIntegration{
			Type:         integrations.TypeSlack,
			Name:         "Slack — nowhere",
			Installation: &integrations.Installation{Agent: "U0BOT"},
		})
	if !errors.Is(err, integrations.ErrInvalidInstallation) {
		t.Fatalf("an installation naming nothing = %v, want ErrInvalidInstallation", err)
	}
}

func TestDisconnectingTakesTheRoutingRecordWithIt(t *testing.T) {
	t.Parallel()

	// The routing record is part of the Integration rather than a dependent of it.
	// Leaving one behind would leave a workspace claimed by a row that is gone, and the
	// customer could not reconnect.
	placements, organization := migratedPlacement(t)
	installed := slackInstallation("T0ACME")
	created, err := connectSlack(t, placements, organization, "Slack — Acme", installed)
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}
	if err := placements.DeleteIntegration(context.Background(), ownerOf(t, organization),
		organization, created.ID); err != nil {
		t.Fatalf("disconnecting: %v", err)
	}

	_, _, err = placements.IntegrationByInstallation(context.Background(),
		integrations.TypeSlack, installed.Key())
	if !errors.Is(err, integrations.ErrUnknown) {
		t.Errorf("a disconnected workspace still resolves: %v", err)
	}

	// And the workspace can be connected again, which is the point of removing it.
	if _, err := connectSlack(t, placements, organization, "Slack — again",
		slackInstallation("T0ACME")); err != nil {
		t.Errorf("reconnecting a disconnected workspace: %v", err)
	}
}

// A second tenant cannot take a workspace the first holds, and learns nothing about who
// does. The refusal is the same one a same-tenant duplicate gets.
func TestAnotherTenantCannotTakeAConnectedWorkspace(t *testing.T) {
	t.Parallel()

	placements, first, second := twoOrganizationsOnOnePlacement(t)

	installed := slackInstallation("T0SHARED-" + uuid.NewString()[:8])
	if _, err := connectSlack(t, placements, first, "Slack — first", installed); err != nil {
		t.Fatalf("connecting slack in the first tenant: %v", err)
	}
	_, err := connectSlack(t, placements, second, "Slack — second",
		slackInstallation(installed.Workspace))
	if !errors.Is(err, integrations.ErrWorkspaceTaken) {
		t.Fatalf("a neighbour claiming the same workspace = %v, want ErrWorkspaceTaken", err)
	}
}

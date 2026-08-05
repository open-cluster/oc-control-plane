package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Every operator-facing store function refuses a principal with no membership in the
// organization it names.
//
// It is a TABLE over the functions rather than a test per function, so a new one is covered by
// code that already exists — and the count is asserted, so a function added to the surface and
// not listed here is a gap somebody has to close deliberately rather than an absence nobody
// sees.
//
// This duplicates the authorization middleware on purpose. The middleware covers every route,
// from a table a gate proves complete; this covers every CALL, including one made from a path
// nobody routed through the middleware. A boundary with one enforcement point is a boundary
// that one forgotten call site removes.
func TestBoundary_EveryOperatorStoreFunctionRefusesANonMember(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)

	stranger := aStranger(t)
	ctx := context.Background()

	// Something real to name, so a refusal cannot be the row simply not existing.
	environment, err := placements.EnsureDefaultEnvironment(
		ctx, ownerOf(t, organization), organization)
	if err != nil {
		t.Fatalf("arranging the default environment: %v", err)
	}
	somebody := uuid.New()

	refusals := map[string]func() error{
		"EnsureDefaultEnvironment": func() error {
			_, err := placements.EnsureDefaultEnvironment(ctx, stranger, organization)
			return err
		},
		"ListEnvironments": func() error {
			_, err := placements.ListEnvironments(ctx, stranger, organization, storage.Page{})
			return err
		},
		"CreateEnvironment": func() error {
			_, err := placements.CreateEnvironment(ctx, stranger, organization, "Trespass")
			return err
		},
		"RenameEnvironment": func() error {
			_, err := placements.RenameEnvironment(
				ctx, stranger, organization, environment.ID, "Trespass")
			return err
		},
		"DeleteEnvironment": func() error {
			return placements.DeleteEnvironment(ctx, stranger, organization, environment.ID)
		},
		"ListConnections": func() error {
			_, err := placements.ListConnections(
				ctx, stranger, organization, uuid.Nil, storage.Page{})
			return err
		},
		"CreateConnection": func() error {
			_, err := placements.CreateConnection(ctx, stranger, organization,
				storage.NewConnection{
					Environment: environment.ID, Integration: "alertmanager",
					Name: "trespass", Role: storage.RoleTrigger,
					Locality: storage.LocalityControlPlane, SecretDigest: randomDigest(t),
				})
			return err
		},
		"RotateConnectionSecret": func() error {
			return placements.RotateConnectionSecret(
				ctx, stranger, organization, somebody, randomDigest(t))
		},
		"SetConnectionDisabled": func() error {
			return placements.SetConnectionDisabled(ctx, stranger, organization, somebody, true)
		},
		"ListRelays": func() error {
			_, err := placements.ListRelays(ctx, stranger, organization, storage.Page{})
			return err
		},
		"SessionConflictTrail": func() error {
			_, err := placements.SessionConflictTrail(
				ctx, stranger, organization, somebody, storage.Page{})
			return err
		},
		"ClearSessionConflict": func() error {
			_, err := placements.ClearSessionConflict(ctx, stranger, organization, somebody)
			return err
		},
		"ListMembers": func() error {
			_, err := placements.ListMembers(ctx, stranger, organization, storage.Page{})
			return err
		},
		"SetMembership": func() error {
			_, err := placements.SetMembership(
				ctx, stranger, organization, somebody, authz.Viewer)
			return err
		},
		"RemoveMembership": func() error {
			return placements.RemoveMembership(ctx, stranger, organization, somebody)
		},
		"ListIdentityProviders": func() error {
			_, err := placements.ListIdentityProviders(ctx, stranger, organization)
			return err
		},
		"ConfigureIdentityProvider": func() error {
			_, err := placements.ConfigureIdentityProvider(ctx, stranger, organization,
				storage.NewIdentityProvider{
					Name: "trespass", Protocol: storage.ProtocolOIDC,
					Issuer: "https://idp.example.test", ClientID: "trespass",
					ClientSecretSealed: []byte("sealed"),
				})
			return err
		},
		"UpdateIdentityProvider": func() error {
			_, err := placements.UpdateIdentityProvider(ctx, stranger, organization, somebody,
				storage.NewIdentityProvider{Name: "trespass"})
			return err
		},
		"RemoveIdentityProvider": func() error {
			return placements.RemoveIdentityProvider(ctx, stranger, organization, somebody)
		},
		"ListSessions": func() error {
			_, err := placements.ListSessions(ctx, stranger, organization)
			return err
		},
		"RevokeSessionsOf": func() error {
			_, err := placements.RevokeSessionsOf(ctx, stranger, organization, somebody)
			return err
		},
		"SetSessionPolicy": func() error {
			return placements.SetSessionPolicy(ctx, stranger, organization, time.Hour, 30)
		},
		"ListServiceAccounts": func() error {
			_, err := placements.ListServiceAccounts(ctx, stranger, organization)
			return err
		},
		"CreateServiceAccount": func() error {
			_, err := placements.CreateServiceAccount(ctx, stranger, organization, "trespass", "")
			return err
		},
		"RemoveServiceAccount": func() error {
			return placements.RemoveServiceAccount(ctx, stranger, organization, somebody)
		},
		"ListAPITokens": func() error {
			_, err := placements.ListAPITokens(ctx, stranger, organization)
			return err
		},
		"IssueAPIToken": func() error {
			_, err := placements.IssueAPIToken(ctx, stranger, organization, storage.NewAPIToken{
				ServiceAccountID: somebody, Digest: randomDigest(t), Prefix: "occp_ab",
				Role: authz.Viewer, ExpiresAt: time.Now().Add(time.Hour),
			})
			return err
		},
		"RevokeAPIToken": func() error {
			return placements.RevokeAPIToken(ctx, stranger, organization, somebody)
		},
		"AuditEvents": func() error {
			_, err := placements.AuditEvents(ctx, stranger, organization, audit.Page{})
			return err
		},
		"OpenInvestigation": func() error {
			_, err := placements.OpenInvestigation(ctx, stranger, organization, investigation.New{
				Scope: investigation.Scope{
					Connection: somebody, Namespace: "default",
					WorkloadKind: investigation.WorkloadDeployment, WorkloadName: "trespass",
				},
				Window: investigation.Window{
					Start: time.Now().Add(-time.Hour), End: time.Now(),
				},
				Trigger: investigation.Trigger{
					Kind: investigation.TriggerManual, RequestedBy: "stranger", At: time.Now(),
				},
			})
			return err
		},
		"CancelInvestigation": func() error {
			return placements.CancelInvestigation(ctx, stranger, organization, somebody)
		},
		"OpenRound": func() error {
			_, err := placements.OpenRound(ctx, stranger, organization,
				investigation.Opening{InvestigationID: somebody})
			return err
		},

		// The provisioning surface. A directory's credential is bound to one organization by
		// the token that carries it, and these are the layer that refuses if it somehow were
		// not — which matters more here than anywhere else on this surface, because this
		// credential lives in a customer's identity vendor.
		"ProvisionedUsers": func() error {
			_, err := placements.ProvisionedUsers(
				ctx, stranger, organization, storage.ProvisionedUserFilter{}, 1, 10)
			return err
		},
		"ProvisionedUser": func() error {
			_, err := placements.ProvisionedUser(ctx, stranger, organization, somebody)
			return err
		},
		"ProvisionUser": func() error {
			_, err := placements.ProvisionUser(ctx, stranger, organization,
				storage.NewProvisionedUser{UserName: "trespass@example.test", Active: true})
			return err
		},
		"ReplaceProvisionedUser": func() error {
			_, err := placements.ReplaceProvisionedUser(ctx, stranger, organization, somebody,
				storage.NewProvisionedUser{UserName: "trespass@example.test", Active: true})
			return err
		},
		"SetProvisionedUserActive": func() error {
			_, err := placements.SetProvisionedUserActive(
				ctx, stranger, organization, somebody, false)
			return err
		},
		"DeprovisionUser": func() error {
			return placements.DeprovisionUser(ctx, stranger, organization, somebody)
		},
		"DirectoryGroups": func() error {
			_, err := placements.DirectoryGroups(ctx, stranger, organization, "", 1, 10)
			return err
		},
		"DirectoryGroup": func() error {
			_, err := placements.DirectoryGroup(ctx, stranger, organization, somebody)
			return err
		},
		"CreateDirectoryGroup": func() error {
			_, err := placements.CreateDirectoryGroup(
				ctx, stranger, organization, "trespass", "", nil)
			return err
		},
		"ReplaceDirectoryGroup": func() error {
			_, err := placements.ReplaceDirectoryGroup(
				ctx, stranger, organization, somebody, "trespass", "", nil)
			return err
		},
		"ChangeDirectoryGroupMembers": func() error {
			_, err := placements.ChangeDirectoryGroupMembers(
				ctx, stranger, organization, somebody, nil, nil)
			return err
		},
		"MapDirectoryGroupToRole": func() error {
			_, err := placements.MapDirectoryGroupToRole(
				ctx, stranger, organization, somebody, authz.Viewer)
			return err
		},
		"DeleteDirectoryGroup": func() error {
			return placements.DeleteDirectoryGroup(ctx, stranger, organization, somebody)
		},
	}

	for name, call := range refusals {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, storage.ErrNotAMember) {
				t.Errorf("storage.%s answered %v for a principal with no membership; the "+
					"boundary must refuse before the work rather than depend on the middleware "+
					"having run", name, err)
			}
		})
	}

	// A gate on the gate. A function added to the operator surface and not listed above would
	// leave this table quietly smaller, and nothing would say so.
	const covered = 45
	if len(refusals) != covered {
		t.Errorf("this table covers %d store functions and expects %d; a function added to the "+
			"operator surface has to be added here too, or the boundary it crosses is untested",
			len(refusals), covered)
	}
}

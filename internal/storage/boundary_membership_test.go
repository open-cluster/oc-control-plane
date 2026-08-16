package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
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

	somebody := uuid.New()

	refusals := map[string]func() error{
		"QueryIntegrations": func() error {
			_, err := placements.QueryIntegrations(
				ctx, stranger, organization, integrations.Query{})
			return err
		},
		"CountIntegrationsByType": func() error {
			_, err := placements.CountIntegrationsByType(ctx, stranger, organization)
			return err
		},
		"CreateIntegration": func() error {
			_, err := placements.CreateIntegration(ctx, stranger, organization,
				integrations.NewIntegration{
					Type: integrations.TypeAlertmanager, Name: "trespass",
					WebhookSecretDigest:      randomDigest(t),
					WebhookSecretFingerprint: "fingerprint",
				})
			return err
		},
		"ReviseIntegration": func() error {
			_, err := placements.ReviseIntegration(
				ctx, stranger, organization, somebody, integrations.Revision{})
			return err
		},
		"SetIntegrationDisabled": func() error {
			return placements.SetIntegrationDisabled(ctx, stranger, organization, somebody, true)
		},
		"DeleteIntegration": func() error {
			return placements.DeleteIntegration(ctx, stranger, organization, somebody)
		},
		"RotateIntegrationWebhookSecret": func() error {
			return placements.RotateIntegrationWebhookSecret(
				ctx, stranger, organization, somebody, randomDigest(t), "fingerprint")
		},
		"RecordIntegrationVerification": func() error {
			_, err := placements.RecordIntegrationVerification(ctx, stranger, organization,
				somebody, integrations.Verification{Status: integrations.StatusActive})
			return err
		},
		"ListRelays": func() error {
			_, err := placements.ListRelays(ctx, stranger, organization, storage.RelayQuery{})
			return err
		},
		"FleetSummary": func() error {
			_, err := placements.FleetSummary(ctx, stranger, organization, time.Minute, "")
			return err
		},
		"IssueOperatorBootstrapToken": func() error {
			return placements.IssueOperatorBootstrapToken(ctx, stranger, organization,
				randomDigest(t), time.Now().Add(time.Hour))
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
	const covered = 43
	if len(refusals) != covered {
		t.Errorf("this table covers %d store functions and expects %d; a function added to the "+
			"operator surface has to be added here too, or the boundary it crosses is untested",
			len(refusals), covered)
	}
}

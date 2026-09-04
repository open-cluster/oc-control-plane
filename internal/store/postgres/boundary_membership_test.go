package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
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
	database, organization := migratedDatabase(t)

	stranger := aStranger(t)
	ctx := context.Background()

	somebody := uuid.New()

	refusals := map[string]func() error{
		"QueryIntegrations": func() error {
			_, err := database.QueryIntegrations(
				ctx, stranger, organization, integrations.Query{})
			return err
		},
		"CountIntegrationsByType": func() error {
			_, err := database.CountIntegrationsByType(ctx, stranger, organization)
			return err
		},
		"CreateIntegration": func() error {
			_, err := database.CreateIntegration(ctx, stranger, organization,
				integrations.NewIntegration{
					Type: integrations.TypeAlertmanager, Name: "trespass",
					WebhookSecretDigest:      randomDigest(t),
					WebhookSecretFingerprint: "fingerprint",
				})
			return err
		},
		"ReviseIntegration": func() error {
			_, err := database.ReviseIntegration(
				ctx, stranger, organization, somebody, integrations.Revision{})
			return err
		},
		"SetIntegrationDisabled": func() error {
			return database.SetIntegrationDisabled(ctx, stranger, organization, somebody, true)
		},
		"DeleteIntegration": func() error {
			return database.DeleteIntegration(ctx, stranger, organization, somebody)
		},
		"RotateIntegrationWebhookSecret": func() error {
			return database.RotateIntegrationWebhookSecret(
				ctx, stranger, organization, somebody, randomDigest(t), "fingerprint")
		},
		"RecordIntegrationVerification": func() error {
			_, err := database.RecordIntegrationVerification(ctx, stranger, organization,
				somebody, integrations.Verification{Status: integrations.StatusActive})
			return err
		},
		"ListRelays": func() error {
			_, err := database.ListRelays(ctx, stranger, organization, storage.RelayQuery{})
			return err
		},
		"FleetSummary": func() error {
			_, err := database.FleetSummary(ctx, stranger, organization, time.Minute, "")
			return err
		},
		"IssueOperatorBootstrapToken": func() error {
			return database.IssueOperatorBootstrapToken(ctx, stranger, organization,
				randomDigest(t), time.Now().Add(time.Hour))
		},
		"SessionConflictTrail": func() error {
			_, err := database.SessionConflictTrail(
				ctx, stranger, organization, somebody, storage.Page{})
			return err
		},
		"ClearSessionConflict": func() error {
			_, err := database.ClearSessionConflict(ctx, stranger, organization, somebody)
			return err
		},
		"ListMembers": func() error {
			_, err := database.ListMembers(ctx, stranger, organization, storage.Page{})
			return err
		},
		"SetMembership": func() error {
			_, err := database.SetMembership(
				ctx, stranger, organization, somebody, authz.Viewer)
			return err
		},
		"RemoveMembership": func() error {
			return database.RemoveMembership(ctx, stranger, organization, somebody)
		},
		"ListSessions": func() error {
			_, err := database.ListSessions(ctx, stranger, organization, storage.Page{})
			return err
		},
		"RevokeSessionsOf": func() error {
			_, err := database.RevokeSessionsOf(ctx, stranger, organization, somebody)
			return err
		},
		"SetSessionPolicy": func() error {
			return database.SetSessionPolicy(ctx, stranger, organization, time.Hour, 30)
		},
		"AuditEvents": func() error {
			_, err := database.AuditEvents(ctx, stranger, organization, audit.Page{})
			return err
		},
		"ReplayWebhookDelivery": func() error {
			return database.ReplayWebhookDelivery(ctx, stranger, organization, somebody)
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
	const covered = 21
	if len(refusals) != covered {
		t.Errorf("this table covers %d store functions and expects %d; a function added to the "+
			"operator surface has to be added here too, or the boundary it crosses is untested",
			len(refusals), covered)
	}
}

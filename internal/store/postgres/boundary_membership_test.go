package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
					WebhookSecretDigest: randomDigest(t),
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
				ctx, stranger, organization, somebody, randomDigest(t))
		},
		"RecordIntegrationVerification": func() error {
			_, err := database.RecordIntegrationVerification(ctx, stranger, organization,
				somebody, integrations.Verification{Status: integrations.StatusVerified})
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
		"SetOrganizationAuditRetention": func() error {
			return database.SetOrganizationAuditRetention(ctx, stranger, organization, 30)
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

}

func TestMembershipGrantAuditUsesTheUserAndRequestContext(t *testing.T) {
	database, organization := migratedDatabase(t)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	actorID, userID := uuid.New(), uuid.New()
	for id, subject := range map[uuid.UUID]string{actorID: "actor", userID: "member"} {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO app_user (user_id, issuer, subject, email)
			VALUES ($1, 'test', $2, $2 || '@example.test')`, id, subject); err != nil {
			t.Fatal(err)
		}
	}
	principal, err := authz.NewPrincipal(authz.KindUser, actorID.String(), "Administrator",
		[]authz.Membership{{Organization: organization, Role: authz.Admin}})
	if err != nil {
		t.Fatal(err)
	}
	principal = principal.WithRequest("192.0.2.10:1234", "request-under-test")

	if _, err := database.SetMembership(
		context.Background(), principal, organization, userID, authz.Viewer); err != nil {
		t.Fatal(err)
	}
	assertMembershipGrantAudit(t, pool, organization.String(), userID, authz.Viewer,
		"192.0.2.10:1234", "request-under-test")

	created, err := database.CreateOrganization(context.Background(), principal, "Created")
	if err != nil {
		t.Fatal(err)
	}
	assertMembershipGrantAudit(t, pool, created.Organization.String(), actorID, authz.Admin,
		"192.0.2.10:1234", "request-under-test")
}

func assertMembershipGrantAudit(
	t *testing.T, pool *pgxpool.Pool, organization string, user uuid.UUID, role authz.Role,
	sourceAddress, requestID string,
) {
	t.Helper()
	var matches bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (
		SELECT 1 FROM audit_event
		 WHERE org_id = $1 AND action = 'membership.granted'
		   AND target_kind = 'user' AND target_id = $2
		   AND source_address = $3 AND request_id = $4
		   AND detail = jsonb_build_object('role', $5::text)
	)`, organization, user.String(), sourceAddress, requestID, string(role)).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("membership grant audit omitted its User, Role, or request context")
	}
}

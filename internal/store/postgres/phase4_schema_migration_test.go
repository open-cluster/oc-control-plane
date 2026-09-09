package storage_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSchemaOwnershipMigrationMovesPolicyAndTightensTenantRoots(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	baseline, err := os.ReadFile("migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `CREATE TABLE schema_migration(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migration(version) VALUES ('0001_baseline');
INSERT INTO organization(org_id,display_name,created_by) VALUES ('retained-org','Retained','operator');
INSERT INTO organization_policy(org_id,session_lifetime_seconds,audit_retention_days,updated_by)
VALUES ('retained-org',0,45,'operator');
INSERT INTO app_user(user_id,issuer,subject,email) VALUES ('11111111-1111-1111-1111-111111111111','local','admin','admin@example.test');
INSERT INTO organization_membership(membership_id,org_id,user_id,role,source)
VALUES ('22222222-2222-2222-2222-222222222222','retained-org','11111111-1111-1111-1111-111111111111','admin',1);
INSERT INTO operator_session(session_id,token_digest,user_id,org_id,issued_at,expires_at,last_seen_at)
VALUES ('33333333-3333-3333-3333-333333333333',decode(repeat('01',32),'hex'),'11111111-1111-1111-1111-111111111111','retained-org',now(),now()+interval '1 hour',now());
INSERT INTO relay_bootstrap_token(token_digest,org_id,expires_at)
VALUES (decode(repeat('02',32),'hex'),'retained-org',now()+interval '1 hour');
INSERT INTO integration(integration_id,org_id,integration_type_id,name)
VALUES ('44444444-4444-4444-4444-444444444444','retained-org',1,'Alertmanager');
INSERT INTO incident(incident_id,org_id,integration_id,grouping_key,grouping_basis,title,status,first_seen_at,last_seen_at,alert_event_count)
VALUES ('55555555-5555-5555-5555-555555555555','retained-org','44444444-4444-4444-4444-444444444444','group',1,'Incident',1,now()-interval '1 hour',now(),99);
INSERT INTO alert_event(alert_event_id,org_id,integration_id,source_key,status,title,summary,started_at,incident_id)
VALUES ('66666666-6666-6666-6666-666666666666','retained-org','44444444-4444-4444-4444-444444444444','one',1,'Alert','Summary',now()-interval '1 hour','55555555-5555-5555-5555-555555555555');
`); err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var organizationID uuid.UUID
	var retention int
	if err := connection.QueryRow(ctx, `SELECT organization_id, audit_retention_days FROM organization WHERE org_id='retained-org'`).
		Scan(&organizationID, &retention); err != nil {
		t.Fatal(err)
	}
	if organizationID == uuid.Nil || retention != 45 {
		t.Fatalf("organization identity/retention = %s/%d", organizationID, retention)
	}
	var integrationOrganizationID uuid.UUID
	if err := connection.QueryRow(ctx, `SELECT organization_id FROM integration WHERE org_id='retained-org'`).
		Scan(&integrationOrganizationID); err != nil {
		t.Fatal(err)
	}
	if integrationOrganizationID != organizationID {
		t.Fatalf("integration Organization UUID = %s, want %s", integrationOrganizationID, organizationID)
	}
	if _, err := connection.Exec(ctx, `UPDATE organization SET org_id='renamed-org' WHERE org_id='retained-org'`); err == nil {
		t.Fatal("Organization selector identity was mutable")
	}
	if _, err := connection.Exec(ctx, `UPDATE organization SET organization_id=$1 WHERE org_id='retained-org'`, uuid.New()); err == nil {
		t.Fatal("Organization UUID identity was mutable")
	}
	for _, removed := range []string{
		`SELECT to_regclass('organization_policy') IS NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='operator_session' AND column_name='token_digest')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='relay_bootstrap_token' AND column_name='token_digest')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='incident' AND column_name='alert_event_count')`,
	} {
		var ok bool
		if err := connection.QueryRow(ctx, removed).Scan(&ok); err != nil || !ok {
			t.Fatalf("phase4 contraction missing for %s: %v", removed, err)
		}
	}
	if _, err := connection.Exec(ctx, `INSERT INTO integration(integration_id,org_id,integration_type_id,name)
VALUES ($1,'missing-org',1,'bad')`, uuid.New()); err == nil {
		t.Fatal("tenant child row without an Organization was accepted")
	}
	if _, err := connection.Exec(ctx, `INSERT INTO integration(integration_id,org_id,organization_id,integration_type_id,name)
VALUES ($1,'retained-org',$2,1,'bad uuid')`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("tenant child row with a mismatched Organization UUID was accepted")
	}
}

func TestSchemaOwnershipMigrationPreservesNullMembershipRolesAsViewer(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	baseline, err := os.ReadFile("migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `CREATE TABLE schema_migration(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migration(version) VALUES ('0001_baseline');
INSERT INTO organization(org_id,display_name,created_by) VALUES ('retained-org','Retained','operator');
INSERT INTO app_user(user_id,issuer,subject,email) VALUES ('11111111-1111-1111-1111-111111111111','local','admin','admin@example.test');
INSERT INTO organization_membership(membership_id,org_id,user_id,source)
VALUES ('22222222-2222-2222-2222-222222222222','retained-org','11111111-1111-1111-1111-111111111111',1);`); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := connection.QueryRow(ctx, `SELECT role FROM organization_membership WHERE org_id='retained-org'`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "viewer" {
		t.Fatalf("retained null role became %q, want least-privilege viewer", role)
	}
}

func TestSchemaOwnershipMigrationRefusesRetainedOrganizationSessionLifetime(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	baseline, err := os.ReadFile("migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `CREATE TABLE schema_migration(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migration(version) VALUES ('0001_baseline');
INSERT INTO organization(org_id,display_name,created_by) VALUES ('retained-org','Retained','operator');
INSERT INTO organization_policy(org_id,session_lifetime_seconds,audit_retention_days,updated_by)
VALUES ('retained-org',900,30,'operator');`); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err == nil {
		t.Fatal("migration silently accepted an Organization-owned session lifetime")
	}
}

func TestOrganizationAuditRetentionUsesOrganizationValue(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	org := organization(t, "org-a")
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err := connection.Exec(ctx, `INSERT INTO organization(org_id,display_name,created_by)
VALUES ($1,'Organization A','test')`, org.String()); err != nil {
		t.Fatal(err)
	}
	if err := database.SetOrganizationAuditRetention(ctx, ownerOf(t, org), org, 30); err != nil {
		t.Fatal(err)
	}
	retention, err := database.OrganizationAuditRetention(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if retention != 30 {
		t.Fatalf("audit retention = %d, want Organization-owned value", retention)
	}
}

func TestSchemaOwnershipFreshInstallHasPhase4Owners(t *testing.T) {
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

	for _, assertion := range []string{
		`SELECT to_regclass('organization_policy') IS NULL`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='organization_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='audit_retention_days')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='integration' AND column_name='organization_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='relay_registration' AND column_name='organization_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='operator_session' AND column_name='credential_digest')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='relay_bootstrap_token' AND column_name='bootstrap_digest')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='incident' AND column_name='alert_event_count')`,
	} {
		var ok bool
		if err := connection.QueryRow(ctx, assertion).Scan(&ok); err != nil || !ok {
			t.Fatalf("fresh schema assertion failed for %s: %v", assertion, err)
		}
	}
	if _, err := connection.Exec(ctx, `INSERT INTO relay_registration
		(registration_id, org_id, credential_digest, cluster_fingerprint, relay_version, capabilities)
		VALUES ($1, 'missing-org', decode(repeat('01',32),'hex'), 'fingerprint', 'test', '{}'::jsonb)`,
		uuid.New()); err == nil {
		t.Fatal("fresh schema accepted a tenant-root row without an Organization")
	}
}

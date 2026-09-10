package storage_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

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

func TestFreshSchemaUsesOneFinalBaseline(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated migration applied %v: %v", applied, err)
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	wantTables := []string{
		"alert_event", "app_user", "audit_event", "change_ledger", "change_ledger_scope",
		"conversation", "conversation_message", "deployment_initialization",
		"deployment_sign_in_flow", "incident", "integration", "integration_connect_flow",
		"integration_installation", "investigation", "investigation_event",
		"investigation_tool_run", "local_password", "organization", "organization_membership",
		"postmortem", "relay_bootstrap_token", "relay_job", "relay_registration",
		"schema_migration", "session", "slack_conversation", "slack_reply",
		"webhook_delivery", "webhook_job",
	}
	var tables []string
	if err := connection.QueryRow(ctx, `SELECT array_agg(tablename ORDER BY tablename)
		FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("fresh tables = %v, want %v", tables, wantTables)
	}

	for _, assertion := range []string{
		`SELECT count(*) = 1 FROM schema_migration`,
		`SELECT to_regclass('organization_policy') IS NULL`,
		`SELECT to_regclass('integration_type') IS NULL`,
		`SELECT to_regclass('operator_session') IS NULL`,
		`SELECT to_regclass('integration_delivery') IS NULL`,
		`SELECT to_regclass('webhook_work') IS NULL`,
		`SELECT to_regclass('relay_session_conflict_event') IS NULL`,
		`SELECT to_regclass('session') IS NOT NULL`,
		`SELECT to_regclass('webhook_delivery') IS NOT NULL`,
		`SELECT to_regclass('webhook_job') IS NOT NULL`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='org_id' AND data_type='uuid')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='org_id' AND data_type<>'uuid')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='organization_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='audit_retention_days')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='session' AND column_name='credential_digest')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='relay_bootstrap_token' AND column_name='bootstrap_digest')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='incident' AND column_name='alert_event_count')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_tables t WHERE schemaname='public' AND NOT EXISTS
			(SELECT 1 FROM pg_constraint c WHERE c.conrelid=(quote_ident(t.schemaname)||'.'||quote_ident(t.tablename))::regclass AND c.contype='p'))`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='org_id'
			AND (data_type <> 'uuid' OR (table_name NOT IN ('audit_event','session') AND is_nullable <> 'NO')))`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='updated_at' AND column_default IS NOT NULL)`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns col WHERE col.table_schema='public' AND col.column_name='org_id'
			AND col.table_name <> 'organization' AND NOT EXISTS (
				SELECT 1 FROM pg_constraint c JOIN unnest(c.conkey) key(attnum) ON true
				JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=key.attnum
				WHERE c.contype='f' AND c.conrelid=col.table_name::regclass AND a.attname='org_id'))`,
	} {
		var ok bool
		if err := connection.QueryRow(ctx, assertion).Scan(&ok); err != nil || !ok {
			t.Fatalf("fresh schema assertion failed for %s: %v", assertion, err)
		}
	}
	var generatedID uuid.UUID
	var retention int
	if err := connection.QueryRow(ctx, `INSERT INTO organization (display_name,created_by)
		VALUES ('Defaulted','test') RETURNING org_id,audit_retention_days`).Scan(&generatedID, &retention); err != nil {
		t.Fatal(err)
	}
	if generatedID == uuid.Nil || retention != 90 {
		t.Fatalf("Organization defaults = %s/%d, want generated UUID and 90-day retention", generatedID, retention)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO relay_registration
		(registration_id, org_id, credential_digest, cluster_fingerprint, relay_version, capabilities)
		VALUES ($1, $2, decode(repeat('01',32),'hex'), 'fingerprint', 'test', '{}'::jsonb)`,
		uuid.New(), uuid.New()); err == nil {
		t.Fatal("fresh schema accepted a tenant-root row without an Organization")
	}
	if _, err := connection.Exec(ctx, `INSERT INTO app_user (user_id,issuer,subject,email)
		VALUES ($1,'test','missing-update-time','test@example.test')`, uuid.New()); err == nil {
		t.Fatal("fresh schema defaulted caller-owned updated_at")
	}
}

func TestBaselineSerializesConcurrentStartup(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	first, second := openDatabaseForTest(t, dsn), openDatabaseForTest(t, dsn)
	results := make(chan []string, 2)
	errors := make(chan error, 2)
	var started sync.WaitGroup
	started.Add(2)
	for _, database := range []*storage.Database{first, second} {
		go func() {
			started.Done()
			started.Wait()
			applied, err := database.Migrate(ctx)
			results <- applied
			errors <- err
		}()
	}
	applied := 0
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		applied += len(<-results)
	}
	if applied != 1 {
		t.Fatalf("concurrent startup applied %d migrations, want one", applied)
	}
}

func TestBaselineFailureRollsBackCompletely(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE TABLE organization (conflict boolean)`); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	if _, err = database.Migrate(ctx); err == nil {
		t.Fatal("conflicting baseline unexpectedly succeeded")
	}
	var rolledBack bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('alert_event') IS NULL
		AND to_regclass('schema_migration') IS NULL`).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("failed baseline left partial schema: %v", err)
	}
}

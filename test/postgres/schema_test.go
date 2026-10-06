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
VALUES ($1,'Organization A','test')`, org); err != nil {
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

func TestFreshSchemaUsesCurrentContract(t *testing.T) {
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
		"alert_event", "app_user", "audit_event", "change_event", "change_scope",
		"conversation", "conversation_message", "incident", "integration",
		"integration_connect_flow", "integration_installation", "investigation", "investigation_event",
		"investigation_tool_run", "local_password", "oidc_sign_in_flow", "organization",
		"organization_membership", "postmortem", "relay_bootstrap_token", "relay_job", "relay_registration",
		"schema_migration", "session", "slack_conversation", "slack_message_work",
		"slack_reply", "webhook_delivery",
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
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name='investigation_tool_run' AND column_name='hypothesis_id')`,
		`SELECT to_regclass('deployment_initialization') IS NULL`,
		`SELECT to_regclass('deployment_sign_in_flow') IS NULL`,
		`SELECT to_regclass('oidc_sign_in_flow') IS NOT NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='integration' AND column_name IN
			('labels','verify_facts','verify_note','disabled_at','created_by','updated_at','credential_fingerprint','webhook_secret_fingerprint'))`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name IN ('oidc_sign_in_flow','integration_connect_flow')
			AND column_name IN ('flow_id','created_at','consumed_at'))`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='integration' AND column_name='verification_grants' AND data_type='ARRAY')`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['integration_id','org_id','provider','name','configuration','relay_id',
			      'credential_sealed','webhook_secret_digest','verification_status','verified_at',
			      'verification_grants','disabled','created_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='integration'`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['org_id','integration_id','provider','installation_key','provider_actor_id']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='integration_installation'`,
		`SELECT to_regclass('organization_policy') IS NULL`,
		`SELECT to_regclass('integration_type') IS NULL`,
		`SELECT to_regclass('operator_session') IS NULL`,
		`SELECT to_regclass('integration_delivery') IS NULL`,
		`SELECT to_regclass('webhook_work') IS NULL`,
		`SELECT to_regclass('relay_session_conflict_event') IS NULL`,
		`SELECT to_regclass('change_ledger') IS NULL`,
		`SELECT to_regclass('change_ledger_scope') IS NULL`,
		`SELECT to_regclass('session') IS NOT NULL`,
		`SELECT to_regclass('webhook_delivery') IS NOT NULL`,
		`SELECT to_regclass('slack_message_work') IS NOT NULL`,
		`SELECT to_regclass('webhook_job') IS NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'webhook_delivery' AND column_name = 'request_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='org_id' AND data_type='uuid')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='org_id' AND data_type<>'uuid')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND column_name='organization_id')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='organization' AND column_name='audit_retention_days')`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='session' AND column_name='credential_digest')`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['user_id','issuer','subject','email','display_name','disabled_at','created_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='app_user'`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['user_id','password_hash','changed_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='local_password'`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['session_id','credential_digest','user_id','expires_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='session'`,
		`SELECT column_default = 'gen_random_uuid()' FROM information_schema.columns
			WHERE table_schema='public' AND table_name='app_user' AND column_name='user_id'`,
		`SELECT column_default = 'gen_random_uuid()' FROM information_schema.columns
			WHERE table_schema='public' AND table_name='session' AND column_name='session_id'`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='oidc_sign_in_flow' AND column_name='org_id')`,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint
			WHERE conrelid='organization_membership'::regclass
			  AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (user_id)')`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) = ARRAY['org_id','user_id','role','created_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='organization_membership'`,
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
	applied := make([]int, 0, 2)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		applied = append(applied, len(<-results))
	}
	if (applied[0] == 0) == (applied[1] == 0) {
		t.Fatalf("concurrent startup applied migration batches of sizes %v, want exactly one initializer", applied)
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

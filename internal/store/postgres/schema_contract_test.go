package storage_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	githubintegration "github.com/open-cluster/oc-control-plane/internal/integrations/github"
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
		`SELECT count(*) = 9 FROM schema_migration`,
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
		`SELECT to_regclass('webhook_job') IS NOT NULL`,
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
			ARRAY['session_id','credential_digest','user_id','issued_at','expires_at',
			      'last_seen_at','revoked_at','client_user_agent','remote_addr']
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

func TestIdentityRowCleanupMigrationPreservesCurrentIdentity(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	_, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES
			('0001_schema'), ('0002_simplify_integrations'), ('0003_simplify_deliveries_and_sessions'),
			('0004_simplify_membership_lifecycle'), ('0005_remove_membership_identity');
		CREATE TABLE app_user (
			user_id uuid PRIMARY KEY, issuer text NOT NULL, subject text NOT NULL, email text NOT NULL,
			email_verified boolean NOT NULL, display_name text NOT NULL, disabled_at timestamptz,
			last_sign_in timestamptz, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL
		);
		CREATE TABLE local_password (
			user_id uuid PRIMARY KEY, password_hash text NOT NULL,
			password_changed_at timestamptz NOT NULL, updated_at timestamptz NOT NULL
		);
		CREATE TABLE session (
			session_id uuid PRIMARY KEY, credential_digest bytea NOT NULL, user_id uuid NOT NULL,
			org_id uuid, issued_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
			last_seen_at timestamptz NOT NULL, revoked_at timestamptz, remote_addr text NOT NULL
		);
		INSERT INTO app_user VALUES
			('10000000-0000-0000-0000-000000000001','issuer','subject','user@example.test',true,
			 'User',NULL,'2026-01-02T00:00:00Z','2026-01-01T00:00:00Z','2026-01-03T00:00:00Z');
		INSERT INTO local_password VALUES
			('10000000-0000-0000-0000-000000000001',repeat('x',32),
			 '2026-01-01T00:00:00Z','2026-01-04T00:00:00Z');
		INSERT INTO session VALUES
			('20000000-0000-0000-0000-000000000001',decode(repeat('01',32),'hex'),
			 '10000000-0000-0000-0000-000000000001',NULL,'2026-01-01T00:00:00Z',
			 '2026-02-01T00:00:00Z','2026-01-02T00:00:00Z',NULL,'192.0.2.10:443');`)
	if err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil || !reflect.DeepEqual(applied, []string{
		"0006_simplify_identity_rows", "0007_readable_integration_provider",
		"0008_contract_provider_installation", "0009_single_organization_identity",
	}) {
		t.Fatalf("applied = %v, error = %v", applied, err)
	}

	var email, displayName, passwordHash, remoteAddr string
	var changedAt, createdAt time.Time
	var clientUserAgent *string
	if err = connection.QueryRow(ctx, `SELECT email,display_name,created_at FROM app_user`).
		Scan(&email, &displayName, &createdAt); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT password_hash,changed_at FROM local_password`).
		Scan(&passwordHash, &changedAt); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT client_user_agent,remote_addr FROM session`).
		Scan(&clientUserAgent, &remoteAddr); err != nil {
		t.Fatal(err)
	}
	if email != "user@example.test" || displayName != "User" ||
		!createdAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		passwordHash != strings.Repeat("x", 32) ||
		!changedAt.Equal(time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)) ||
		clientUserAgent != nil || remoteAddr != "192.0.2.10:443" {
		t.Fatalf("identity rows not preserved: email=%q name=%q created=%s password=%q changed=%s agent=%v remote=%q",
			email, displayName, createdAt, passwordHash, changedAt, clientUserAgent, remoteAddr)
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
	if applied != 9 {
		t.Fatalf("concurrent startup applied %d migrations, want nine", applied)
	}
}

func TestReadableProviderMigrationMapsEveryCurrentProvider(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	_, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES
			('0001_schema'), ('0002_simplify_integrations'), ('0003_simplify_deliveries_and_sessions'),
			('0004_simplify_membership_lifecycle'), ('0005_remove_membership_identity'),
			('0006_simplify_identity_rows');
		CREATE TABLE integration (
			integration_id uuid PRIMARY KEY, org_id uuid NOT NULL, integration_type_id smallint NOT NULL,
			name text NOT NULL, configuration jsonb NOT NULL DEFAULT '{}', webhook_secret_digest bytea,
			relay_id uuid, verification_status text, verified_at timestamptz,
			verification_grants text[] NOT NULL DEFAULT '{}', disabled boolean NOT NULL DEFAULT false,
			created_at timestamptz NOT NULL DEFAULT now(), credential_sealed bytea,
			CONSTRAINT integration_identity_is_org_scoped UNIQUE (org_id,integration_id),
			CONSTRAINT integration_org_id_kind_unique UNIQUE (org_id,integration_id,integration_type_id)
		);
		CREATE TABLE integration_installation (
			integration_id uuid PRIMARY KEY, org_id uuid NOT NULL, integration_type_id smallint NOT NULL,
			application text NOT NULL, enterprise text NOT NULL DEFAULT '', workspace text NOT NULL,
			enterprise_wide boolean NOT NULL DEFAULT false, agent text NOT NULL DEFAULT '',
			authorizer text NOT NULL DEFAULT '', installed_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL,
			CONSTRAINT integration_installation_matches_parent_kind
				FOREIGN KEY (org_id,integration_id,integration_type_id)
				REFERENCES integration(org_id,integration_id,integration_type_id)
		);
		CREATE UNIQUE INDEX integration_installation_is_one_workspace
			ON integration_installation(integration_type_id,application,enterprise,workspace);
		INSERT INTO integration(integration_id,org_id,integration_type_id,name,configuration)
		SELECT ('10000000-0000-0000-0000-00000000000' || kind)::uuid,
			('20000000-0000-0000-0000-00000000000' || kind)::uuid,
			kind, 'provider-' || kind, jsonb_build_object('kind', kind)
		FROM generate_series(1,5) kind;
		INSERT INTO integration_installation
			(integration_id,org_id,integration_type_id,application,workspace,updated_at)
		VALUES ('10000000-0000-0000-0000-000000000003','20000000-0000-0000-0000-000000000003',
			3,'A1','T1',now());`)
	if err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil || !reflect.DeepEqual(applied, []string{
		"0007_readable_integration_provider", "0008_contract_provider_installation",
		"0009_single_organization_identity",
	}) {
		t.Fatalf("applied = %v, error = %v", applied, err)
	}

	rows, err := connection.Query(ctx, `SELECT org_id::text,provider,configuration->>'kind'
		FROM integration ORDER BY integration_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{"alertmanager", "kubernetes", "slack", "github", "generic_webhook"}
	index := 0
	for rows.Next() {
		if index == len(want) {
			t.Fatal("migration produced more Integration rows than it received")
		}
		var organization, provider, configurationKind string
		if err := rows.Scan(&organization, &provider, &configurationKind); err != nil {
			t.Fatal(err)
		}
		kind := strconv.Itoa(index + 1)
		if organization != "20000000-0000-0000-0000-00000000000"+kind ||
			provider != want[index] || configurationKind != kind {
			t.Fatalf("migrated row %d = %q/%q/%q", index, organization, provider, configurationKind)
		}
		index++
	}
	if err := rows.Err(); err != nil || index != len(want) {
		t.Fatalf("read %d migrated rows: %v", index, err)
	}
	var exactIntegrationRow bool
	if err := connection.QueryRow(ctx, `SELECT array_agg(column_name::text ORDER BY column_name) =
		ARRAY['configuration','created_at','credential_sealed','disabled','integration_id','name',
		      'org_id','provider','relay_id','verification_grants','verification_status','verified_at',
		      'webhook_secret_digest']
		FROM information_schema.columns WHERE table_schema='public' AND table_name='integration'`).
		Scan(&exactIntegrationRow); err != nil || !exactIntegrationRow {
		t.Fatalf("migrated Integration row is not exact: %v", err)
	}
	var installationProvider string
	if err := connection.QueryRow(ctx, `SELECT provider FROM integration_installation`).
		Scan(&installationProvider); err != nil || installationProvider != "slack" {
		t.Fatalf("installation provider = %q, error = %v", installationProvider, err)
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

func TestCompatibilityMigrationPreservesProviderInstallationIdentity(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	_, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES ('0001_schema');
		CREATE TABLE integration (
			integration_id uuid PRIMARY KEY, org_id uuid NOT NULL, integration_type_id smallint NOT NULL,
			name text NOT NULL, configuration jsonb NOT NULL DEFAULT '{}', webhook_secret_digest bytea,
			webhook_secret_fingerprint text, webhook_secret_created_at timestamptz, webhook_secret_rotated_at timestamptz,
			labels jsonb NOT NULL DEFAULT '{}', relay_id uuid, status smallint NOT NULL DEFAULT 1,
			last_verified_at timestamptz, verify_note text NOT NULL DEFAULT '', disabled_at timestamptz,
			created_by text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL,
			credential_sealed bytea, credential_fingerprint text, credential_created_at timestamptz,
			credential_rotated_at timestamptz, verify_grants jsonb, verify_facts jsonb,
			CONSTRAINT integration_credential_is_whole CHECK (true),
			CONSTRAINT integration_status_check CHECK (status = ANY (ARRAY[1,2,3,4])),
			CONSTRAINT integration_supported_kind CHECK (integration_type_id = ANY (ARRAY[1,2,3,4,5])),
			CONSTRAINT integration_webhook_secret_is_whole CHECK (true),
			CONSTRAINT integration_name_is_unique_per_org UNIQUE (org_id,name)
		);
		CREATE TABLE integration_installation (
			integration_id uuid PRIMARY KEY, org_id uuid NOT NULL, integration_type_id smallint NOT NULL,
			application text NOT NULL, enterprise text NOT NULL DEFAULT '', workspace text NOT NULL,
			enterprise_wide boolean NOT NULL DEFAULT false, agent text NOT NULL DEFAULT '',
			authorizer text NOT NULL DEFAULT '', grants text[] NOT NULL DEFAULT '{}',
			installed_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL,
			CONSTRAINT integration_installation_supported_kind CHECK (integration_type_id = ANY (ARRAY[1,2,3,4,5]))
		);
		CREATE UNIQUE INDEX integration_installation_is_one_workspace
			ON integration_installation (integration_type_id, application, enterprise, workspace);
		INSERT INTO integration(integration_id,org_id,integration_type_id,name,configuration,status,last_verified_at,
			updated_at,verify_grants,verify_facts) VALUES
			('10000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001',3,'Slack',
			 '{"appID":"A1","teamId":"T1"}',2,now(),now(),'["channels:read"]','{"botUserId":"U1"}'),
			('10000000-0000-0000-0000-000000000002','20000000-0000-0000-0000-000000000001',4,'GitHub',
			 '{"installationId":77}',2,now(),now(),'[]','{"account":"acme"}'),
			('10000000-0000-0000-0000-000000000003','20000000-0000-0000-0000-000000000001',4,'GitHub duplicate',
			 '{"installationId":77}',2,now(),now(),'[]','{"account":"acme"}');`)
	if err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil || !reflect.DeepEqual(applied, []string{
		"0002_simplify_integrations", "0003_simplify_deliveries_and_sessions",
		"0004_simplify_membership_lifecycle", "0005_remove_membership_identity",
		"0006_simplify_identity_rows", "0007_readable_integration_provider",
		"0008_contract_provider_installation", "0009_single_organization_identity",
	}) {
		t.Fatalf("applied = %v, error = %v", applied, err)
	}

	rows, err := connection.Query(ctx, `SELECT provider,installation_key,provider_actor_id
		FROM integration_installation ORDER BY provider`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type migratedInstallation struct {
		provider string
		key      []string
		actor    *string
	}
	var got []migratedInstallation
	for rows.Next() {
		var installed migratedInstallation
		if err := rows.Scan(&installed.provider, &installed.key, &installed.actor); err != nil {
			t.Fatal(err)
		}
		got = append(got, installed)
	}
	if len(got) != 2 || got[0].provider != "github" ||
		!reflect.DeepEqual(got[0].key, []string{"77"}) || got[0].actor != nil ||
		got[1].provider != "slack" ||
		!reflect.DeepEqual(got[1].key, []string{"A1", "T1"}) ||
		got[1].actor == nil || *got[1].actor != "U1" {
		t.Fatalf("installations = %v", got)
	}
	routed, _, err := database.IntegrationByInstallation(ctx, "slack",
		integrations.InstallationKey{"A1", "T1"})
	if err != nil || routed.ID != uuid.MustParse("10000000-0000-0000-0000-000000000001") ||
		routed.OrgID != "20000000-0000-0000-0000-000000000001" {
		t.Fatalf("migrated Slack routing = %+v, %v", routed, err)
	}
	var configurations []string
	if err := connection.QueryRow(ctx, `SELECT array_agg(configuration::text ORDER BY provider) FROM integration`).Scan(&configurations); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configurations, []string{"{}", "{}", "{}"}) {
		t.Fatalf("editable configurations = %v", configurations)
	}
	duplicate, found, err := database.InstallationOf(ctx,
		organization(t, "20000000-0000-0000-0000-000000000001"),
		uuid.MustParse("10000000-0000-0000-0000-000000000003"))
	if err != nil || found {
		t.Fatalf("ambiguous legacy installation survived as %+v: %v", duplicate, err)
	}
	var contracted bool
	if err := connection.QueryRow(ctx, `SELECT
		(SELECT pg_get_constraintdef(oid) = 'PRIMARY KEY (org_id, integration_id)'
		 FROM pg_constraint WHERE conrelid='integration_installation'::regclass AND contype='p')
		AND EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname='public'
		 AND indexname='integration_installation_provider_key_unique'
		 AND indexdef LIKE '%UNIQUE INDEX% (provider, installation_key)')
		AND EXISTS (SELECT 1 FROM pg_constraint
		 WHERE conrelid='integration_installation'::regclass
		 AND conname='integration_installation_matches_parent_provider'
		 AND pg_get_constraintdef(oid) =
		 'FOREIGN KEY (org_id, integration_id, provider) REFERENCES integration(org_id, integration_id, provider)')`).
		Scan(&contracted); err != nil || !contracted {
		t.Fatalf("Provider Installation constraints are not exact: %v", err)
	}

	org := organization(t, "20000000-0000-0000-0000-000000000001")
	loaded, err := database.Integration(ctx, org,
		uuid.MustParse("10000000-0000-0000-0000-000000000002"))
	if err != nil {
		t.Fatal(err)
	}
	tool := githubintegration.Definition(nil, githubintegration.NewClient("")).Tools[0]
	_, err = tool.Run(ctx, integrations.ToolRequest{Integration: loaded})
	if !errors.Is(err, githubintegration.ErrNoApp) {
		t.Fatalf("migrated GitHub identity did not reach Tool execution: %v", err)
	}
}

func TestDeliveryAndSessionCleanupMigrationPreservesAcceptedWork(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	_, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES ('0001_schema'), ('0002_simplify_integrations');
		CREATE TABLE webhook_delivery (
			delivery_id uuid PRIMARY KEY, org_id uuid NOT NULL, integration_id uuid NOT NULL,
			outcome smallint NOT NULL, body_digest bytea, reason text NOT NULL DEFAULT '',
			alert_event_count integer NOT NULL DEFAULT 0, truncated integer NOT NULL DEFAULT 0,
			received_at timestamptz NOT NULL DEFAULT now(), provider_identity text,
			lifecycle_phase text, request_id text NOT NULL DEFAULT '',
			CONSTRAINT webhook_delivery_identity_is_org_scoped UNIQUE (org_id, delivery_id)
		);
		CREATE UNIQUE INDEX webhook_delivery_accepted_provider_identity_is_unique
			ON webhook_delivery (integration_id, provider_identity, lifecycle_phase) WHERE outcome = 1;
		CREATE INDEX webhook_delivery_accepted_idx
			ON webhook_delivery (integration_id, received_at DESC) WHERE outcome = 1;
		CREATE INDEX webhook_delivery_integration_idx
			ON webhook_delivery (org_id, integration_id, received_at DESC, delivery_id DESC);
		CREATE TABLE webhook_job (
			job_id uuid PRIMARY KEY, org_id uuid NOT NULL, delivery_id uuid NOT NULL,
			CONSTRAINT webhook_job_delivery_is_in_the_same_org FOREIGN KEY (org_id, delivery_id)
				REFERENCES webhook_delivery(org_id, delivery_id)
		);
		CREATE TABLE session (
			session_id uuid PRIMARY KEY, revoked_by text NOT NULL DEFAULT '',
			user_agent text NOT NULL DEFAULT '', address text NOT NULL DEFAULT ''
		);
		INSERT INTO webhook_delivery
			(delivery_id, org_id, integration_id, outcome, body_digest, reason,
			 alert_event_count, truncated, provider_identity, lifecycle_phase, request_id)
		VALUES
			('30000000-0000-0000-0000-000000000001', '20000000-0000-0000-0000-000000000001',
			 '10000000-0000-0000-0000-000000000001', 1, decode(repeat('11',32),'hex'), '',
			 1, 7, 'event-1', 'firing', 'request-1'),
			('30000000-0000-0000-0000-000000000002', '20000000-0000-0000-0000-000000000001',
			 '10000000-0000-0000-0000-000000000001', 2, NULL, '', 0, 0, NULL, NULL, ''),
			('30000000-0000-0000-0000-000000000003', '20000000-0000-0000-0000-000000000001',
			 '10000000-0000-0000-0000-000000000001', 3, NULL, 'malformed', 0, 0, NULL, NULL, '');
		INSERT INTO webhook_job(job_id, org_id, delivery_id)
		VALUES ('40000000-0000-0000-0000-000000000001', '20000000-0000-0000-0000-000000000001',
			'30000000-0000-0000-0000-000000000001');
		INSERT INTO session(session_id, revoked_by, user_agent, address)
		VALUES ('50000000-0000-0000-0000-000000000001', 'actor', 'browser', '127.0.0.1:8080');`)
	if err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil || !reflect.DeepEqual(applied, []string{
		"0003_simplify_deliveries_and_sessions", "0004_simplify_membership_lifecycle",
		"0005_remove_membership_identity", "0006_simplify_identity_rows",
		"0007_readable_integration_provider", "0008_contract_provider_installation",
		"0009_single_organization_identity",
	}) {
		t.Fatalf("applied = %v, error = %v", applied, err)
	}

	var deliveries, jobs, truncated int
	var deliveryID, digest string
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM webhook_delivery`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT delivery_id::text, encode(content_digest, 'hex'), truncated
		FROM webhook_delivery`).Scan(&deliveryID, &digest, &truncated); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || deliveryID != "30000000-0000-0000-0000-000000000001" ||
		digest != "1111111111111111111111111111111111111111111111111111111111111111" || truncated != 7 {
		t.Fatalf("accepted delivery after migration = %d/%s/%s/%d", deliveries, deliveryID, digest, truncated)
	}
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM webhook_job`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("dependent Webhook Jobs = %d, error = %v", jobs, err)
	}

	var contracted bool
	if err = connection.QueryRow(ctx, `SELECT
		NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='webhook_delivery'
			AND column_name IN ('outcome','reason','body_digest','alert_event_count'))
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='webhook_delivery'
			AND column_name='content_digest' AND is_nullable='NO')
		AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='session'
			AND column_name IN ('revoked_by','user_agent','address'))
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='session'
			AND column_name IN ('client_user_agent','remote_addr') GROUP BY table_name HAVING count(*)=2)`).Scan(&contracted); err != nil || !contracted {
		t.Fatalf("cleanup schema contract = %t, error = %v", contracted, err)
	}
	var clientUserAgent *string
	var remoteAddr string
	if err = connection.QueryRow(ctx, `SELECT client_user_agent,remote_addr FROM session`).
		Scan(&clientUserAgent, &remoteAddr); err != nil || clientUserAgent != nil || remoteAddr != "127.0.0.1:8080" {
		t.Fatalf("session metadata = %v/%q, error = %v", clientUserAgent, remoteAddr, err)
	}
	if _, err = connection.Exec(ctx, `UPDATE webhook_delivery SET content_digest = decode('01','hex')`); err == nil {
		t.Fatal("migrated schema accepted a content digest that is not SHA-256 sized")
	}
	if _, err = connection.Exec(ctx, `INSERT INTO webhook_delivery
		(delivery_id,org_id,integration_id,content_digest,truncated,provider_identity,lifecycle_phase)
		VALUES ('30000000-0000-0000-0000-000000000004','20000000-0000-0000-0000-000000000001',
		'10000000-0000-0000-0000-000000000001',decode(repeat('22',32),'hex'),0,'event-1','firing')`); err == nil {
		t.Fatal("migrated schema accepted a reused provider identity and lifecycle phase")
	}
}

func TestMembershipCleanupMigrationPreservesOnlyCurrentRelations(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	_, err = connection.Exec(ctx, `
		CREATE TABLE schema_migration (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES
			('0001_schema'), ('0002_simplify_integrations'), ('0003_simplify_deliveries_and_sessions');
		CREATE TABLE organization (org_id uuid PRIMARY KEY);
		CREATE TABLE app_user (user_id uuid PRIMARY KEY);
		CREATE TABLE organization_membership (
			membership_id uuid PRIMARY KEY, org_id uuid NOT NULL, user_id uuid NOT NULL,
			role text NOT NULL, source smallint NOT NULL, external_id text, active boolean NOT NULL,
			granted_by text NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			CONSTRAINT organization_membership_role_check CHECK (role = ANY (ARRAY['admin','editor','viewer'])),
			CONSTRAINT organization_membership_source_check CHECK (source = ANY (ARRAY[1,2,3])),
			CONSTRAINT organization_membership_is_one_per_tenant UNIQUE (org_id,user_id),
			CONSTRAINT organization_membership_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id),
			CONSTRAINT organization_membership_user_id_fkey FOREIGN KEY (user_id) REFERENCES app_user(user_id)
		);
		CREATE UNIQUE INDEX organization_membership_external_id_is_unique_per_org
			ON organization_membership (org_id,external_id) WHERE external_id IS NOT NULL;
		INSERT INTO organization VALUES ('20000000-0000-0000-0000-000000000001');
		INSERT INTO app_user VALUES
			('30000000-0000-0000-0000-000000000001'),
			('30000000-0000-0000-0000-000000000002');
		INSERT INTO organization_membership VALUES
			('40000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001',
			 '30000000-0000-0000-0000-000000000001','admin',1,NULL,true,'actor','2026-01-01','2026-01-02'),
			('40000000-0000-0000-0000-000000000002','20000000-0000-0000-0000-000000000001',
			 '30000000-0000-0000-0000-000000000002','viewer',1,NULL,false,'actor','2026-01-03','2026-01-04');`)
	if err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil || !reflect.DeepEqual(applied, []string{
		"0004_simplify_membership_lifecycle", "0005_remove_membership_identity",
		"0006_simplify_identity_rows", "0007_readable_integration_provider",
		"0008_contract_provider_installation", "0009_single_organization_identity",
	}) {
		t.Fatalf("applied = %v, error = %v", applied, err)
	}

	var users, memberships int
	var role string
	var created time.Time
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT count(*),min(role),min(created_at)
		FROM organization_membership`).Scan(&memberships, &role, &created); err != nil {
		t.Fatal(err)
	}
	if users != 2 || memberships != 1 || role != "admin" ||
		!created.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("migrated Memberships = users:%d rows:%d role:%q created:%s",
			users, memberships, role, created)
	}

	var columns, primaryKey []string
	if err = connection.QueryRow(ctx, `SELECT array_agg(column_name ORDER BY ordinal_position)
		FROM information_schema.columns
		WHERE table_schema='public' AND table_name='organization_membership'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT array_agg(attribute.attname ORDER BY key.ordinality)
		FROM pg_constraint constraint_record
		CROSS JOIN LATERAL unnest(constraint_record.conkey) WITH ORDINALITY AS key(attnum,ordinality)
		JOIN pg_attribute attribute ON attribute.attrelid=constraint_record.conrelid AND attribute.attnum=key.attnum
		WHERE constraint_record.conrelid='organization_membership'::regclass
		  AND constraint_record.contype='p'`).Scan(&primaryKey); err != nil {
		t.Fatal(err)
	}
	wantColumns := []string{"org_id", "user_id", "role", "created_at"}
	if !reflect.DeepEqual(columns, wantColumns) || !reflect.DeepEqual(primaryKey, []string{"org_id", "user_id"}) {
		t.Fatalf("Membership contract = columns:%v primary key:%v", columns, primaryKey)
	}
	var constraints int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid='organization_membership'::regclass
		  AND conname IN ('organization_membership_role_check',
		                  'organization_membership_organization_exists',
		                  'organization_membership_user_id_fkey')`).Scan(&constraints); err != nil || constraints != 3 {
		t.Fatalf("retained Membership constraints = %d, error = %v", constraints, err)
	}
}

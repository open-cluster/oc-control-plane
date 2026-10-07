package storage_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestIssue150MigrationConvertsSupportedRetainedData(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection := baselineDatabase(t, ctx, dsn)
	defer func() { _ = connection.Close(ctx) }()

	org, conversationID, investigationID := uuid.New(), uuid.New(), uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO organization(org_id, display_name, created_by)
		VALUES ($1, 'Organization', 'test')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO conversation(conversation_id, org_id, surface, subject)
		VALUES ($2, $1, 1, 'Checkout')`, org, conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO investigation(
			investigation_id, org_id, question, subject, window_from, window_until, status,
			conclusion, concluded_at, conversation_id, turn)
		VALUES ($3, $1, 'Why did checkout fail?', 'Checkout', now() - interval '1 hour', now(), 2,
			'{"hypotheses":[{"id":"legacy","statement":"Deploy","status":"exploring","test":"Read history","runRefs":[]}]}'::jsonb,
			now(), $2, 1)`, org, conversationID, investigationID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO conversation_message(
			conversation_id, org_id, sequence, role, actor_kind, actor_id, text, investigation_id,
			window_from, window_until)
		VALUES ($2, $1, 1, 1, 1, 'user-1', 'Why did checkout fail?', $3, now() - interval '1 hour', now());
		`, org, conversationID, investigationID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO investigation_event(investigation_id, org_id, sequence, type, payload) VALUES
			($2, $1, 1, 1, '{"question":"legacy"}'),
			($2, $1, 2, 2, '{"text":"Reading","message":"legacy"}'),
			($2, $1, 3, 5, '{"legacy":true}'),
			($2, $1, 4, 10, '{"hypotheses":[]}'),
			($2, $1, 5, 4, '{"ordinal":1,"outcome":"failed","error":"provider token sk-secret-value"}'),
			($2, $1, 6, 4, '{"ordinal":2,"outcome":"failed","error":"not one of the tools the selected sources offer"}'),
			($2, $1, 7, 3, '{"ordinal":3,"tool":"github.read","integrationId":"00000000-0000-0000-0000-000000000001","integration":"GitHub","purpose":"Read evidence","arguments":{"token":"secret"},"hypothesisId":"legacy"}'),
			($2, $1, 8, 6, '{"status":"answer_only","summary":"The answer","question":"legacy"}'),
			($2, $1, 9, 7, '{"reason":"Model failed","error":"legacy"}'),
			($2, $1, 10, 9, '{"message":"Cancelled","reason":"legacy"}'),
			($2, $1, 11, 4, '{"ordinal":4,"outcome":"failed","error":"not executed: token sk-secret-value"}')`, org, investigationID); err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	applied, err := database.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"0002_simplify_conversation_investigation_contracts"}) {
		t.Fatalf("applied migrations = %v", applied)
	}

	var source string
	var conclusion string
	if err = connection.QueryRow(ctx, `SELECT source FROM conversation WHERE conversation_id=$1`, conversationID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT conclusion::text FROM investigation WHERE investigation_id=$1`, investigationID).Scan(&conclusion); err != nil {
		t.Fatal(err)
	}
	if source != "web" || strings.Contains(conclusion, `"id"`) || !strings.Contains(conclusion, `"status": "unresolved"`) {
		t.Fatalf("converted source/conclusion = %q/%s", source, conclusion)
	}
	expectedPayloads := map[int]map[string]any{
		1: {},
		2: {"text": "Reading"},
		5: {"ordinal": float64(1), "outcome": "failed", "durationMs": float64(0),
			"summary": "Tool failed", "truncated": false},
		6: {"ordinal": float64(2), "outcome": "failed", "durationMs": float64(0),
			"summary": "not one of the tools the selected sources offer", "truncated": false},
		7: {"ordinal": float64(3), "tool": "github.read",
			"integrationId": "00000000-0000-0000-0000-000000000001",
			"integration":   "GitHub", "purpose": "Read evidence"},
		8:  {"status": "answer_only", "summary": "The answer"},
		9:  {"reason": "Model failed"},
		10: {"message": "Cancelled"},
		11: {"ordinal": float64(4), "outcome": "failed", "durationMs": float64(0),
			"summary": "Tool failed", "truncated": false},
	}
	rows, err := connection.Query(ctx, `SELECT sequence,payload::text FROM investigation_event
		WHERE investigation_id=$1 AND type IN (1,2,3,4,6,7,9) ORDER BY sequence`, investigationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seenPayloads := make(map[int]bool, len(expectedPayloads))
	for rows.Next() {
		var sequence int
		var raw string
		if err = rows.Scan(&sequence, &raw); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload, expectedPayloads[sequence]) {
			t.Errorf("sequence %d payload = %#v, want %#v", sequence, payload, expectedPayloads[sequence])
		}
		seenPayloads[sequence] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seenPayloads) != len(expectedPayloads) {
		t.Fatalf("validated active payloads = %v, want sequences %v", seenPayloads, expectedPayloads)
	}
	var retained int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM investigation_event WHERE investigation_id=$1 AND type IN (5,10)`, investigationID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		t.Fatalf("retained retired events = %d, want 2", retained)
	}
	if _, err = connection.Exec(ctx, `INSERT INTO investigation_event(investigation_id,org_id,sequence,type) VALUES ($1,$2,5,10)`, investigationID, org); err == nil {
		t.Fatal("migration accepted a new retired event type")
	}
}

func TestIssue150MigrationRefusesAmbiguousRetainedDataWithoutMutation(t *testing.T) {
	tests := []struct {
		name, kind, want string
	}{
		{
			name: "source conflicts with attribution",
			kind: "attribution",
			want: "retained user attribution conflicts",
		},
		{
			name: "question has no authoritative message",
			kind: "question",
			want: "retained nonempty questions",
		},
		{
			name: "question message belongs to another conversation",
			kind: "cross-conversation-question",
			want: "retained nonempty questions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dsn := postgresDSN(t)
			connection := baselineDatabase(t, ctx, dsn)
			defer func() { _ = connection.Close(ctx) }()
			org, recordID := uuid.New(), uuid.New()
			if _, err := connection.Exec(ctx, `INSERT INTO organization(org_id,display_name,created_by)
				VALUES ($1,'Organization','test')`, org); err != nil {
				t.Fatal(err)
			}
			if test.kind == "attribution" {
				if _, err := connection.Exec(ctx, `INSERT INTO conversation(conversation_id,org_id,surface,subject)
					VALUES ($2,$1,1,'Checkout')`, org, recordID); err != nil {
					t.Fatal(err)
				}
				if _, err := connection.Exec(ctx, `INSERT INTO conversation_message(
					conversation_id,org_id,sequence,role,actor_kind,text,window_from,window_until)
					VALUES ($2,$1,1,1,2,'ambiguous',now()-interval '1 hour',now())`, org, recordID); err != nil {
					t.Fatal(err)
				}
			} else {
				conversationID := uuid.Nil
				if test.kind == "cross-conversation-question" {
					conversationID = uuid.New()
					otherConversation := uuid.New()
					if _, err := connection.Exec(ctx, `INSERT INTO conversation(conversation_id,org_id,surface,subject)
						VALUES ($2,$1,1,'Expected'),($3,$1,1,'Wrong')`, org, conversationID, otherConversation); err != nil {
						t.Fatal(err)
					}
					if _, err := connection.Exec(ctx, `INSERT INTO investigation(
						investigation_id,org_id,question,subject,window_from,window_until,conversation_id,turn)
						VALUES ($2,$1,'Only retained here','Checkout',now()-interval '1 hour',now(),$3,1)`, org, recordID, conversationID); err != nil {
						t.Fatal(err)
					}
					if _, err := connection.Exec(ctx, `INSERT INTO conversation_message(
						conversation_id,org_id,sequence,role,actor_kind,text,investigation_id,window_from,window_until)
						VALUES ($2,$1,1,1,1,'Wrong Conversation',$3,now()-interval '1 hour',now())`, org, otherConversation, recordID); err != nil {
						t.Fatal(err)
					}
				} else if _, err := connection.Exec(ctx, `INSERT INTO investigation(
					investigation_id,org_id,question,subject,window_from,window_until)
					VALUES ($2,$1,'Only retained here','Checkout',now()-interval '1 hour',now())`, org, recordID); err != nil {
					t.Fatal(err)
				}
			}

			database := openDatabaseForTest(t, dsn)
			if _, err := database.Migrate(ctx); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("migration error = %v, want %q", err, test.want)
			}
			var versions []string
			if err := connection.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migration`).Scan(&versions); err != nil {
				t.Fatal(err)
			}
			var oldSchema bool
			if err := connection.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema='public' AND table_name='conversation' AND column_name='surface')`).Scan(&oldSchema); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(versions, []string{"0001_current_schema"}) || !oldSchema {
				t.Fatalf("refused migration mutated schema: versions=%v old_schema=%v", versions, oldSchema)
			}
		})
	}
}

func baselineDatabase(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	baseline, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "internal", "store", "postgres", "migrations", "0001_current_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(ctx, `CREATE TABLE schema_migration
		(version TEXT NOT NULL PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO schema_migration(version) VALUES ('0001_current_schema')`); err != nil {
		t.Fatal(err)
	}
	return connection
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
	applied, err := database.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"0001_current_schema", "0002_simplify_conversation_investigation_contracts"}) {
		t.Fatalf("fresh migration applied %v, want the baseline and issue 150 contraction", applied)
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
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
			ARRAY['conversation_id','org_id','incident_id','source','subject','state',
			      'created_by','created_at','last_activity_at']
			FROM information_schema.columns WHERE table_schema='public' AND table_name='conversation'`,
		`SELECT data_type = 'text' AND column_default = '''web''::text'
			FROM information_schema.columns WHERE table_schema='public' AND table_name='conversation' AND column_name='source'`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='conversation_message' AND column_name='actor_kind')`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='investigation' AND column_name='question')`,
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

func TestUnrecognizedMigrationLedgerIsRefusedWithoutMutation(t *testing.T) {
	for _, version := range []string{"0001_schema", "9999_unknown"} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			dsn := postgresDSN(t)
			connection, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close(ctx) }()
			if _, err = connection.Exec(ctx, `
				CREATE TABLE schema_migration
				(
					version TEXT NOT NULL PRIMARY KEY,
					applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
				);
				CREATE TABLE legacy_marker(value TEXT NOT NULL);
				INSERT INTO legacy_marker(value) VALUES ('preserve me')`); err != nil {
				t.Fatal(err)
			}
			if _, err = connection.Exec(ctx,
				`INSERT INTO schema_migration(version) VALUES ($1)`, version); err != nil {
				t.Fatal(err)
			}

			database := openDatabaseForTest(t, dsn)
			_, err = database.Migrate(ctx)
			if err == nil || !strings.Contains(err.Error(), "pre-release database must be recreated") {
				t.Fatalf("migration error = %v, want actionable recreation refusal", err)
			}
			var marker string
			var versions []string
			if err = connection.QueryRow(ctx, `SELECT value FROM legacy_marker`).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if err = connection.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migration`).Scan(&versions); err != nil {
				t.Fatal(err)
			}
			if marker != "preserve me" || !reflect.DeepEqual(versions, []string{version}) {
				t.Fatalf("refusal mutated database: marker=%q versions=%v", marker, versions)
			}
		})
	}
}

func TestUnledgeredApplicationSchemaIsRefusedWithoutMutation(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE TABLE organization (legacy_value TEXT NOT NULL);
		INSERT INTO organization(legacy_value) VALUES ('preserve me')`); err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	_, err = database.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "pre-release database must be recreated") {
		t.Fatalf("unledgered schema error = %v, want actionable recreation refusal", err)
	}
	var marker string
	var ledgerAbsent bool
	if err = connection.QueryRow(ctx, `SELECT legacy_value FROM organization`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT to_regclass('schema_migration') IS NULL`).Scan(&ledgerAbsent); err != nil {
		t.Fatal(err)
	}
	if marker != "preserve me" || !ledgerAbsent {
		t.Fatalf("refusal mutated unledgered database: marker=%q ledger_absent=%v", marker, ledgerAbsent)
	}
}

func TestEmptyLedgerDoesNotMaskApplicationObjects(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE TABLE schema_migration
		(version TEXT NOT NULL PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE organization (legacy_value TEXT NOT NULL);
		INSERT INTO organization(legacy_value) VALUES ('preserve me')`); err != nil {
		t.Fatal(err)
	}

	database := openDatabaseForTest(t, dsn)
	_, err = database.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "pre-release database must be recreated") {
		t.Fatalf("empty-ledger schema error = %v, want actionable recreation refusal", err)
	}
	var marker string
	var versions int
	if err = connection.QueryRow(ctx, `SELECT legacy_value FROM organization`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM schema_migration`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if marker != "preserve me" || versions != 0 {
		t.Fatalf("refusal mutated empty-ledger database: marker=%q versions=%d", marker, versions)
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

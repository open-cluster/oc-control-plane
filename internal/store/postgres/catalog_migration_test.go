package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestCatalogMigrationPreservesInstallationsAndRejectsConflictingKinds(t *testing.T) {
	ctx := context.Background()
	dsn := postgresDSN(t)
	database := openDatabaseForTest(t, dsn)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	files, err := filepath.Glob("migrations/000*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, "CREATE TABLE schema_migration(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, string(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, "INSERT INTO schema_migration(version) VALUES ($1)", strings.TrimSuffix(filepath.Base(file), ".sql")); err != nil {
			t.Fatal(err)
		}
	}
	id, flow := uuid.New(), uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO organization(org_id, display_name, created_by)
		VALUES ('retained-org', 'Retained Organization', 'operator')`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
 INSERT INTO integration(integration_id, org_id, integration_type_id, name, credential_sealed, credential_fingerprint, credential_created_at)
 VALUES ($1, 'retained-org', 3, 'Retained Slack', '0203', 'fingerprint', now());
 `, id); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO integration_installation(integration_id, org_id, integration_type_id, application, workspace)
 VALUES ($1, 'retained-org', 3, 'app', 'workspace')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO integration_connect_flow(flow_id, org_id, integration_type_id, principal, state_digest, expires_at)
 VALUES ($1, 'retained-org', 4, 'user', decode(repeat('ab',32),'hex'), now()+interval '1 hour')`, flow); err != nil {
		t.Fatal(err)
	}

	for _, invalid := range []struct{ setup, repair string }{
		{`INSERT INTO integration_type(integration_type_id,key,name,description,logo,category) VALUES (99,'unknown','Unknown','','','other')`, `DELETE FROM integration_type WHERE integration_type_id=99`},
		{`UPDATE integration_type SET key='conflicting' WHERE integration_type_id=3`, `UPDATE integration_type SET key='slack' WHERE integration_type_id=3`},
		{`UPDATE integration_installation SET integration_type_id=4`, `UPDATE integration_installation SET integration_type_id=3`},
	} {
		if _, err := connection.Exec(ctx, invalid.setup); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Migrate(ctx); err == nil {
			t.Fatal("conflicting retained data was accepted")
		}
		var catalogRemains bool
		if err := connection.QueryRow(ctx, "SELECT to_regclass('integration_type') IS NOT NULL").Scan(&catalogRemains); err != nil || !catalogRemains {
			t.Fatalf("failed migration changed catalog: %v", err)
		}
		if _, err := connection.Exec(ctx, invalid.repair); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated migration: %v %v", applied, err)
	}
	var preserved bool
	if err := connection.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM integration i JOIN integration_installation s USING (integration_id,org_id,integration_type_id)
 JOIN integration_connect_flow f ON f.org_id=i.org_id
 WHERE i.integration_id=$1 AND f.flow_id=$2 AND i.credential_sealed='0203'::bytea
 AND i.credential_fingerprint='fingerprint' AND s.workspace='workspace' AND f.integration_type_id=4)`, id, flow).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("retained data changed: %v", err)
	}
	for _, statement := range []string{
		"UPDATE integration SET integration_type_id=99",
		"UPDATE integration_connect_flow SET integration_type_id=99",
		"UPDATE integration_installation SET integration_type_id=99",
		"UPDATE integration_installation SET integration_type_id=4",
	} {
		if _, err := connection.Exec(ctx, statement); err == nil {
			t.Fatalf("invalid kind accepted: %s", statement)
		}
	}
}

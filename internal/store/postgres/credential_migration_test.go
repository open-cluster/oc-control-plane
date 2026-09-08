package storage_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestCredentialMigrationPreservesEnvelopesAndRefusesRetainedRefreshData(t *testing.T) {
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
INSERT INTO organization(org_id,display_name,created_by) VALUES ('retained-org','Retained','operator')`); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO integration(integration_id,org_id,integration_type_id,name,
credential_sealed,credential_key_id,credential_fingerprint,credential_created_at)
VALUES ($1,'retained-org',3,'Slack',decode('010203','hex'),'default','fingerprint',now())`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO integration_installation(integration_id,org_id,integration_type_id,application,workspace)
VALUES ($1,'retained-org',3,'app','workspace')`, id); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	for _, retained := range []string{"expires_at=now()", "refresh_sealed=decode('aabb','hex')"} {
		if _, err := connection.Exec(ctx, "UPDATE integration_installation SET "+retained); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Migrate(ctx); err == nil {
			t.Fatal("upgrade discarded retained refresh data")
		}
		var preserved bool
		if err := connection.QueryRow(ctx, `SELECT credential_key_id='default' AND credential_sealed=decode('010203','hex')
FROM integration WHERE integration_id=$1`, id).Scan(&preserved); err != nil || !preserved {
			t.Fatalf("failed upgrade changed credentials: %v", err)
		}
		if _, err := connection.Exec(ctx, "UPDATE integration_installation SET expires_at=NULL, refresh_sealed=NULL"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := connection.QueryRow(ctx, `SELECT i.credential_sealed=decode('010203','hex')
AND i.credential_fingerprint='fingerprint' AND s.workspace='workspace'
FROM integration i JOIN integration_installation s USING (org_id,integration_id)
WHERE i.integration_id=$1`, id).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("upgrade changed credential or routing: %v", err)
	}
	var remaining int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public'
AND ((table_name='integration' AND column_name='credential_key_id')
OR (table_name='integration_installation' AND column_name IN ('expires_at','refresh_sealed')))`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("redundant columns remain: %d, %v", remaining, err)
	}
}

package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAutomaticIncidentInvestigationIsUniqueAndManualWorkCanCoexist(t *testing.T) {
	database, organization, other := twoOrganizationsInOneDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)
	incident := recordIncident(t, database, organization, integration, "automatic")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	insert := func(automatic bool) error {
		_, err := pool.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, incident_id, subject, window_from, window_until,
			 automatic_incident)
			VALUES ($1, $2, $3, 'service', now(), now(), $4)`,
			uuid.New(), organization, incident, automatic)
		return err
	}
	if err := insert(true); err != nil {
		t.Fatalf("first automatic Investigation: %v", err)
	}
	var violation *pgconn.PgError
	if err := insert(true); !errors.As(err, &violation) || violation.Code != "23505" {
		t.Fatalf("second automatic Investigation error = %v, want unique violation", err)
	}
	for range 2 {
		if err := insert(false); err != nil {
			t.Fatalf("manual Investigation on the same Incident: %v", err)
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO investigation
		(investigation_id, org_id, incident_id, subject, window_from, window_until,
		 automatic_incident)
		VALUES ($1, $2, $3, 'service', now(), now(), true)`, uuid.New(), other, incident)
	if !errors.As(err, &violation) || violation.Code != "23503" {
		t.Fatalf("cross-Organization Incident error = %v, want foreign-key violation", err)
	}
}

func TestAutomaticIncidentInvestigationRequiresAnIncidentAndCannotBeAConversationTurn(t *testing.T) {
	database, organization, _ := twoOrganizationsInOneDatabase(t)
	incident := recordIncident(t, database, organization,
		alertmanagerIntegration(t, database, organization), "shape")
	chat := openConversation(t, database, organization, "service")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		incident     any
		conversation any
		turn         any
	}{
		{name: "missing Incident"},
		{name: "Conversation turn", incident: incident, conversation: chat.ID, turn: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), `INSERT INTO investigation
				(investigation_id, org_id, incident_id, conversation_id, turn,
				 subject, window_from, window_until, automatic_incident)
				VALUES ($1, $2, $3, $4, $5, 'service', now(), now(), true)`,
				uuid.New(), organization, test.incident, test.conversation, test.turn)
			var violation *pgconn.PgError
			if !errors.As(err, &violation) || violation.Code != "23514" {
				t.Fatalf("invalid automatic Investigation error = %v, want check violation", err)
			}
		})
	}
}

func TestAutomaticIncidentMigrationPreservesExistingInvestigationsAndOldWriters(t *testing.T) {
	ctx := context.Background()
	database := openDatabaseForTest(t, postgresDSN(t))
	if err := storage.MigrateBeforeAutomaticIncidentForTest(ctx, database); err != nil {
		t.Fatal(err)
	}
	organization := organization(t, "legacy")
	ensureTestOrganization(t, database, organization)
	incident := recordIncident(t, database, organization,
		alertmanagerIntegration(t, database, organization), "legacy")
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	insert := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO investigation
			(investigation_id, org_id, incident_id, subject, window_from, window_until)
			VALUES ($1, $2, $3, 'service', now(), now())`, uuid.New(), organization, incident); err != nil {
			t.Fatal(err)
		}
	}
	insert()
	insert()
	applied, err := database.Migrate(ctx)
	if err != nil || len(applied) != 1 || applied[0] != "0011_automatic_incident_investigation" {
		t.Fatalf("upgrade applied %v: %v", applied, err)
	}
	insert()
	var total, automatic int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE automatic_incident)
		FROM investigation WHERE org_id = $1 AND incident_id = $2`, organization, incident).
		Scan(&total, &automatic); err != nil || total != 3 || automatic != 0 {
		t.Fatalf("legacy and old-writer rows: total=%d automatic=%d err=%v", total, automatic, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE investigation SET automatic_incident = NULL WHERE org_id = $1`, organization); err == nil {
		t.Fatal("automatic Incident marker accepted NULL")
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated migration applied %v: %v", applied, err)
	}
}

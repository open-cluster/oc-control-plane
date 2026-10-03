package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestFreshSlackWorkSchemaContainsOnlySlackState(t *testing.T) {
	database, organization := migratedDatabase(t)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, assertion := range []string{
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND
		 ((table_name = 'investigation' AND column_name = 'webhook_job_id') OR
		  (table_name = 'slack_message_work' AND column_name IN ('incident_id', 'kind'))))`,
		`SELECT count(*) = 2 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'slack_message_work'
		 AND column_name IN ('conversation_id', 'message_sequence') AND is_nullable = 'NO'`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN
		 ('investigation_webhook_job_is_in_the_same_org', 'slack_message_work_incident_is_in_the_same_org',
		  'slack_message_work_kind_check', 'slack_message_work_has_one_effect_reference'))`,
		`SELECT to_regclass('investigation_webhook_job_is_unique') IS NULL`,
		`SELECT to_regclass('webhook_job') IS NULL`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'slack_message_work' AND column_name = 'work_id')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname LIKE 'webhook_job%')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_class WHERE relname LIKE 'webhook_job%')`,
	} {
		var valid bool
		if err := pool.QueryRow(context.Background(), assertion).Scan(&valid); err != nil || !valid {
			t.Fatalf("Slack-only schema contract failed for %s: %v", assertion, err)
		}
	}
}

func TestSlackWorkSchemaRejectsDuplicateAndCrossOrganizationReferences(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	pool, err := fixture.database.Pool(fixture.organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name         string
		organization uuid.UUID
		sequence     any
		code         string
	}{
		{"duplicate Message effect", fixture.organization, int64(1), "23505"},
		{"another Organization", fixture.other, int64(1), "23503"},
		{"missing Message", fixture.organization, nil, "23502"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), `INSERT INTO slack_message_work
				(work_id, org_id, delivery_id, integration_id, conversation_id, message_sequence, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, now())`, uuid.New(), scenario.organization,
				fixture.delivery, fixture.integration, fixture.conversation, scenario.sequence)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != scenario.code {
				t.Fatalf("invalid Slack reference was not rejected with %s: %v", scenario.code, err)
			}
		})
	}
}

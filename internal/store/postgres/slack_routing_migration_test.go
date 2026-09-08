package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSlackRoutingMigrationPreservesProgressAndRefusesConflictingDestinations(t *testing.T) {
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
	integration, conversation, investigation := uuid.New(), uuid.New(), uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO integration(integration_id,org_id,integration_type_id,name)
VALUES ($1,'retained-org',3,'Slack')`, integration); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO conversation(conversation_id,org_id,subject,surface)
VALUES ($1,'retained-org','Retained Slack thread',2)`, conversation); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO investigation(investigation_id,org_id,subject,conversation_id,turn,window_from,window_until)
VALUES ($1,'retained-org','Retained question',$2,1,now()-interval '1 hour',now())`, investigation, conversation); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO slack_conversation(conversation_id,org_id,integration_id,channel_id,thread_ts)
VALUES ($1,'retained-org',$2,'C-ORIGINAL','1700000001.1')`, conversation, integration); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO slack_reply(investigation_id,org_id,integration_id,conversation_id,
channel_id,thread_ts,status,last_sequence,stream_ts,native,attempts,note)
VALUES ($1,'retained-org',$2,$3,'C-WRONG','1700000001.1',1,12,'1700000002.1',true,2,'retry')`, investigation, integration, conversation); err != nil {
		t.Fatal(err)
	}
	database := openDatabaseForTest(t, dsn)
	if _, err := database.Migrate(ctx); err == nil {
		t.Fatal("upgrade accepted a conflicting retained destination")
	}
	var channel string
	if err := connection.QueryRow(ctx, `SELECT channel_id FROM slack_reply WHERE investigation_id=$1`, investigation).Scan(&channel); err != nil || channel != "C-WRONG" {
		t.Fatalf("failed migration changed the retained route: %q %v", channel, err)
	}
	if _, err := connection.Exec(ctx, `UPDATE slack_reply SET channel_id='C-ORIGINAL' WHERE investigation_id=$1`, investigation); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := database.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("repeated migration: %v %v", applied, err)
	}
	reply := claimed(t, database, investigation, time.Minute)
	if reply.Integration != integration || reply.Conversation != conversation || reply.Stream.Channel != "C-ORIGINAL" ||
		reply.Stream.Thread != "1700000001.1" || reply.Stream.TS != "1700000002.1" || !reply.Stream.Native ||
		reply.LastSequence != 12 || reply.Attempts != 2 {
		t.Fatalf("upgrade changed delivery destination or progress: %+v", reply)
	}
	for _, statement := range []string{
		`DELETE FROM slack_conversation WHERE conversation_id=$1`,
		`UPDATE investigation SET conversation_id=NULL,turn=NULL WHERE conversation_id=$1`,
	} {
		if _, err := connection.Exec(ctx, statement, conversation); err == nil {
			t.Fatalf("reply ownership could be removed: %s", statement)
		}
	}
	org := organization(t, "retained-org")
	if err := database.CompleteSlackReply(ctx, org, investigation, reply.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteIntegration(ctx, ownerOf(t, org), org, integration); err != nil {
		t.Fatalf("completed reply prevented disconnection: %v", err)
	}
	if _, _, _, _, found, err := database.SlackReplyState(ctx, org, investigation); err != nil || found {
		t.Fatalf("disconnection retained reply state: found=%v, %v", found, err)
	}
}

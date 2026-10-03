package storage_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
)

type slackMessageFixture struct {
	database     *storage.Database
	organization uuid.UUID
	other        uuid.UUID
	integration  uuid.UUID
	conversation uuid.UUID
	delivery     uuid.UUID
	worker       slackwork.MessageWorker
}

type storedSlackMessageWork struct {
	Status       storage.SlackMessageWorkStatus
	Attempts     int
	FailureClass string
	AvailableAt  time.Time
	UpdatedAt    time.Time
}

func readSlackMessageWork(t *testing.T, fixture slackMessageFixture) storedSlackMessageWork {
	t.Helper()
	pool, err := fixture.database.Pool(fixture.organization)
	if err != nil {
		t.Fatal(err)
	}
	var work storedSlackMessageWork
	if err := pool.QueryRow(context.Background(), `SELECT status, attempts, failure_class, available_at, updated_at
		FROM slack_message_work WHERE org_id = $1 AND conversation_id = $2 AND message_sequence = 1`,
		fixture.organization, fixture.conversation).Scan(&work.Status, &work.Attempts, &work.FailureClass,
		&work.AvailableAt, &work.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	return work
}

func acceptedSlackMessage(t *testing.T) slackMessageFixture {
	t.Helper()
	database, organization, other := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	integration, err := connectSlack(t, database, organization, "Slack", slackInstallation("TWORK"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CWORK", Thread: "1.0", Subject: "accepted question", ActorID: "UWORK", Text: "why?",
	})
	if err != nil {
		t.Fatal(err)
	}
	var delivery uuid.UUID
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_id FROM slack_message_work
		WHERE org_id = $1 AND conversation_id = $2 AND message_sequence = 1`,
		organization, outcome.Conversation).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	return slackMessageFixture{
		database: database, organization: organization, other: other, integration: integration.ID,
		conversation: outcome.Conversation, delivery: delivery,
		worker: slackwork.MessageWorker{
			Database: database, Owner: "slack-message-worker", WindowLead: 2 * time.Hour,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

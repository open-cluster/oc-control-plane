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
	deliveries, err := database.WebhookDeliveries(ctx, organization, "", storage.Page{Limit: 10})
	if err != nil || len(deliveries.Deliveries) != 1 {
		t.Fatalf("accepted delivery: %+v, %v", deliveries, err)
	}
	return slackMessageFixture{
		database: database, organization: organization, other: other, integration: integration.ID,
		conversation: outcome.Conversation, delivery: deliveries.Deliveries[0].ID,
		worker: slackwork.MessageWorker{
			Database: database, Owner: "slack-message-worker", WindowLead: 2 * time.Hour,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

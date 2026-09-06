package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestProviderConversationRequiresAnIntactOriginBinding(t *testing.T) {
	database, org, other := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	browser := openConversation(t, database, org, "browser question")
	if origin, err := database.ConversationOrigin(ctx, org, browser.ID); err != nil || origin != nil {
		t.Fatalf("browser origin=%+v err=%v", origin, err)
	}
	integration, err := connectSlack(t, database, org, "Slack", slackInstallation("TORIGIN"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := database.RecordSlackMessage(ctx, org, storage.SlackMessage{
		Integration: integration.ID, BodyDigest: randomDigest(t), Channel: "CORIGIN", Thread: "1.0",
		Subject: "origin", ActorID: "UORIGIN", Text: "question",
	})
	if err != nil {
		t.Fatal(err)
	}
	origin, err := database.ConversationOrigin(ctx, org, chat.Conversation)
	if err != nil || origin == nil || origin.IntegrationID != integration.ID || origin.Channel != "CORIGIN" || origin.Thread != "1.0" {
		t.Fatalf("verified origin=%+v err=%v", origin, err)
	}
	if _, err := database.ConversationOrigin(ctx, other, chat.Conversation); err == nil {
		t.Fatal("another Organization could resolve the origin")
	}
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE conversation_message RENAME TO unavailable_history`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConversationBrief(ctx, org, chat.Conversation, 10); err == nil {
		t.Fatal("history remained available after its table was renamed")
	}
	independent, err := database.ConversationOrigin(ctx, org, chat.Conversation)
	if err != nil || independent == nil || *independent != *origin {
		t.Fatalf("history failure changed origin: %+v err=%v", independent, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE unavailable_history RENAME TO conversation_message`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM slack_conversation WHERE org_id = $1 AND conversation_id = $2`,
		org.String(), chat.Conversation); err != nil {
		t.Fatal(err)
	}
	if origin, err := database.ConversationOrigin(ctx, org, chat.Conversation); err == nil {
		t.Fatalf("missing provider binding became unrestricted origin: %+v", origin)
	}
}

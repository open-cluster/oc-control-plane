package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackMessageWorkerOpensAcceptedTurnExactlyOnce(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	database, organization := fixture.database, fixture.organization
	ctx := context.Background()
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("acknowledged message before processing: %+v, %v", detail, err)
	}
	worker := fixture.worker
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing accepted message: %v, %v", worked, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("processing completed message again: %v, %v", worked, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 1 {
		t.Fatalf("processed message: %+v, %v", detail, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageComplete || work.Attempts != 1 {
		t.Fatalf("processed work: %+v", work)
	}
	if _, err := database.ConversationDetail(ctx, fixture.other, fixture.conversation, 50); err == nil {
		t.Fatal("another Organization could read the processed Conversation")
	}
}

func TestSlackMessageWorkerQueuesFollowupUntilTheCurrentTurnEnds(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	database, organization, worker := fixture.database, fixture.organization, fixture.worker
	ctx := context.Background()
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("initial message: %v, %v", worked, err)
	}
	if _, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: fixture.integration, ContentDigest: randomDigest(t), Channel: "CWORK", Thread: "1.0",
		Subject: "follow-up", ActorID: "UWORK", Text: "check the next thing",
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("follow-up message: %v, %v", worked, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 2 || !detail.Messages[1].Queued() {
		t.Fatalf("follow-up did not wait for the current turn: %+v, %v", detail, err)
	}
	first := detail.Turns[0].InvestigationID
	if err := database.ConcludeInvestigation(ctx, organization, first, claimToken(t, database, organization, first),
		conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if drained, err := database.DrainQueuedConversation(ctx, worker.WindowLead, 0); err != nil || !drained {
		t.Fatalf("draining the queued follow-up: %v, %v", drained, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 2 || len(detail.Messages) != 2 ||
		detail.Messages[1].InvestigationID != detail.Turns[1].InvestigationID {
		t.Fatalf("queued follow-up lost its next turn: %+v, %v", detail, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("completed follow-up work was reclaimed: %v, %v", worked, err)
	}
}

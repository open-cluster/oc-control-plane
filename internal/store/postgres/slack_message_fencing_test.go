package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackMessageLeaseRejectsOtherOrganizationsAndSupersededOwners(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	first, found, err := database.ClaimSlackMessageWork(ctx, "first-owner", time.Minute)
	if err != nil || !found {
		t.Fatalf("initial claim: %+v, %v, %v", first, found, err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, fixture.other, first, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization renewed the lease: %v", err)
	}
	if err := database.DeferSlackMessageWork(ctx, fixture.other, first, time.Second); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization deferred the message: %v", err)
	}
	if err := database.FailSlackMessageWork(ctx, fixture.other, first, true, 0, "foreign", "foreign"); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization failed the message: %v", err)
	}
	if err := database.ApplySlackMessageWork(ctx, fixture.other, first, time.Hour, 0); err == nil {
		t.Fatal("another Organization applied the Message")
	}
	forged := first
	forged.LeaseEpoch++
	if err := database.HeartbeatSlackMessageWork(ctx, organization, forged, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("incorrect epoch renewed the lease: %v", err)
	}
	forged = first
	forged.LeaseOwner = "incorrect-owner"
	if err := database.HeartbeatSlackMessageWork(ctx, organization, forged, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("incorrect owner renewed the lease: %v", err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, organization, first, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("expired lease was resurrected: %v", err)
	}
	replacement, found, err := database.ClaimSlackMessageWork(ctx, "replacement-owner", time.Minute)
	if err != nil || !found || replacement.ID != first.ID || replacement.LeaseEpoch != first.LeaseEpoch+1 {
		t.Fatalf("replacement claim did not advance fencing: %+v, %v, %v", replacement, found, err)
	}
	for name, attempt := range map[string]func() error{
		"heartbeat": func() error { return database.HeartbeatSlackMessageWork(ctx, organization, first, time.Minute) },
		"deferral":  func() error { return database.DeferSlackMessageWork(ctx, organization, first, time.Second) },
		"failure": func() error {
			return database.FailSlackMessageWork(ctx, organization, first, true, 0, "stale", "stale")
		},
		"source reference": func() error {
			return database.SetSlackMessageSourceReference(ctx, organization, fixture.conversation,
				first.MessageSequence, "https://test.slack.com/archives/stale", first)
		},
		"turn creation": func() error { return database.ApplySlackMessageWork(ctx, organization, first, time.Hour, 0) },
	} {
		if err := attempt(); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
			t.Fatalf("superseded %s was accepted: %v", name, err)
		}
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 || detail.Messages[0].SourceReference != "" {
		t.Fatalf("stale owner left partial effects: %+v, %v", detail, err)
	}
	if err := database.ApplySlackMessageWork(ctx, organization, replacement, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 {
		t.Fatalf("replacement could not open the turn: %+v, %v", detail, err)
	}
}

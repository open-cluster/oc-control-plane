package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// AN ANSWER OWED, AND THE CURSOR THAT MAKES A RETRY SAFE.
//
// Two properties carry the whole design. Every turn of a Slack conversation owes exactly one
// answer, DERIVED from records that already exist rather than hooked into the two places a turn
// can be opened — a hook in one of them is a silent gap in the other. And the cursor only ever
// moves forward, which is what makes a retry append what was missed instead of reposting what
// was already seen.

// aSlackTurn connects a workspace, binds a thread to a conversation and opens a turn on it,
// returning the investigation that now owes an answer.
func aSlackTurn(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	workspace, channel, thread string,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	integration, err := connectSlack(t, placements, organization,
		"Slack — "+workspace, slackInstallation(workspace))
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	outcome, err := placements.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration:  integration.ID,
		BodyDigest:   randomDigest(t),
		Channel:      channel,
		Thread:       thread,
		Subject:      "why is checkout failing?",
		ActorID:      "U9SRE",
		ActorDisplay: "U9SRE",
		Text:         "why is checkout failing?",
	})
	if err != nil {
		t.Fatalf("recording a slack message: %v", err)
	}

	turn, opened, err := placements.OpenTurn(ctx, organization, outcome.Conversation, time.Hour)
	if err != nil {
		t.Fatalf("opening a turn: %v", err)
	}
	if !opened {
		t.Fatal("the first message of a conversation opened no turn")
	}
	return turn.InvestigationID, integration.ID
}

// claimed takes the delivery a worker would be holding. A delivery is only ever advanced by
// the worker that claimed it, so acting on one that was never claimed would be testing a state
// the product does not reach.
func claimed(
	t *testing.T, placements *storage.Placements, investigation uuid.UUID, lease time.Duration,
) {
	t.Helper()

	held, err := placements.ClaimSlackDeliveries(context.Background(), 10, lease)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range held {
		if one.Investigation == investigation {
			return
		}
	}
	t.Fatalf("the delivery for %s was not claimed: %+v", investigation, held)
}

func TestEveryTurnOfASlackConversationOwesAnAnswer(t *testing.T) {
	t.Parallel()

	// Nothing hooked this: the delivery is derived from the investigation and the thread
	// binding, both of which already exist. That is what makes it impossible for a turn
	// opened by the drain behind a running one to be silently missed.
	placements, organization := migratedPlacement(t)
	investigation, integration := aSlackTurn(t, placements, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")

	claimed, err := placements.ClaimSlackDeliveries(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("%d deliveries are owed, want one: %+v", len(claimed), claimed)
	}
	one := claimed[0]
	if one.Investigation != investigation || one.Integration != integration {
		t.Errorf("the delivery names %s through %s, want %s through %s",
			one.Investigation, one.Integration, investigation, integration)
	}
	if one.Channel != "C0INCIDENTS" || one.Thread != "1700000001.1" {
		t.Errorf("the delivery answers into %s/%s, want the thread the question was asked in",
			one.Channel, one.Thread)
	}
	if one.StreamTS != "" || one.LastSequence != 0 {
		t.Errorf("a fresh delivery already claims progress: %+v", one)
	}
}

func TestAConversationOutsideSlackOwesNothing(t *testing.T) {
	t.Parallel()

	// A console conversation has no thread to answer in, and nothing about it should look
	// like an answer owed.
	placements, organization := migratedPlacement(t)
	ctx := context.Background()
	opened, err := placements.OpenConversation(ctx, ownerOf(t, organization), organization,
		conversation.NewConversation{
			Surface: conversation.SurfaceWeb, Subject: "asked in the console",
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a console conversation: %v", err)
	}
	if _, err := placements.AppendMessage(ctx, ownerOf(t, organization), organization,
		opened.ID, conversation.NewMessage{
			Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: "user-under-test", Text: "why is checkout failing?",
		}); err != nil {
		t.Fatalf("saying something: %v", err)
	}
	if _, _, err := placements.OpenTurn(ctx, organization, opened.ID, time.Hour); err != nil {
		t.Fatalf("opening its turn: %v", err)
	}

	claimed, err := placements.ClaimSlackDeliveries(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("a console conversation owes a slack answer: %+v", claimed)
	}
}

func TestAClaimedDeliveryIsNotClaimedTwice(t *testing.T) {
	t.Parallel()

	// The lease is what stops two workers writing into one visible message, which is the
	// one failure a reader in the thread could not make sense of.
	placements, organization := migratedPlacement(t)
	aSlackTurn(t, placements, organization, "T0ACME", "C0INCIDENTS", "1700000001.1")

	first, err := placements.ClaimSlackDeliveries(context.Background(), 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("the first claim = %+v, %v", first, err)
	}
	second, err := placements.ClaimSlackDeliveries(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a leased delivery was claimed again: %+v", second)
	}
}

func TestTheCursorOnlyEverMovesForward(t *testing.T) {
	t.Parallel()

	// The property a retry depends on. A cursor that could go backwards would make a
	// retry repost what the thread has already seen.
	placements, organization := migratedPlacement(t)
	investigation, _ := aSlackTurn(t, placements, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	claimed(t, placements, investigation, time.Minute)

	if err := placements.AdvanceSlackDelivery(ctx, organization, investigation,
		"1700000100.100", true, 12); err != nil {
		t.Fatalf("advancing: %v", err)
	}
	// A later pass that somehow read an older batch must not undo it.
	if err := placements.AdvanceSlackDelivery(ctx, organization, investigation,
		"1700000100.100", true, 4); err != nil {
		t.Fatalf("advancing backwards: %v", err)
	}

	_, sequence, streamTS, _, found, err := placements.SlackDeliveryState(ctx,
		organization, investigation)
	if err != nil || !found {
		t.Fatalf("reading the delivery = %v, found=%v", err, found)
	}
	if sequence != 12 {
		t.Errorf("the cursor is at %d, want it to have stayed at 12", sequence)
	}
	// And the visible message's identity is written once. A second identity would be a
	// second message in the thread.
	if err := placements.AdvanceSlackDelivery(ctx, organization, investigation,
		"1700000999.999", true, 13); err != nil {
		t.Fatalf("advancing: %v", err)
	}
	_, _, again, _, _, err := placements.SlackDeliveryState(ctx, organization, investigation)
	if err != nil {
		t.Fatalf("reading the delivery: %v", err)
	}
	if again != streamTS {
		t.Errorf("the visible message moved from %q to %q", streamTS, again)
	}
}

func TestGivingUpEndsTheDeliveryAndNotTheInvestigation(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	investigation, _ := aSlackTurn(t, placements, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	claimed(t, placements, investigation, time.Minute)

	if err := placements.RetrySlackDelivery(ctx, organization, investigation,
		time.Now(), "slack would not open the reply", true); err != nil {
		t.Fatalf("giving up: %v", err)
	}

	status, _, _, note, found, err := placements.SlackDeliveryState(ctx, organization, investigation)
	if err != nil || !found {
		t.Fatalf("reading the delivery = %v, found=%v", err, found)
	}
	if status != storage.SlackDeliveryFailed {
		t.Errorf("status = %d, want failed", status)
	}
	if note == "" {
		t.Error("giving up recorded no reason an operator could read")
	}
	// And it is never claimed again, because there is nothing left to try.
	claimed, err := placements.ClaimSlackDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range claimed {
		if one.Investigation == investigation {
			t.Error("a delivery that gave up was claimed again")
		}
	}

	// The INVESTIGATION is untouched. A chat outage must not be able to make completed
	// work look failed.
	record, err := placements.Investigation(ctx, organization, investigation)
	if err != nil {
		t.Fatalf("reading the investigation: %v", err)
	}
	if record.ID != investigation {
		t.Errorf("the investigation record is not readable after a failed delivery")
	}
}

func TestADeliveredAnswerIsNeverClaimedAgain(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	investigation, _ := aSlackTurn(t, placements, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	// A lease that has already expired, so what keeps this delivery from being claimed
	// again is its STATE rather than a lease that has not run out yet.
	claimed(t, placements, investigation, -time.Second)

	if err := placements.CompleteSlackDelivery(ctx, organization, investigation); err != nil {
		t.Fatalf("completing: %v", err)
	}
	claimed, err := placements.ClaimSlackDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range claimed {
		if one.Investigation == investigation {
			t.Error("a delivered answer was claimed again, which would repeat it")
		}
	}
}

// Guards the assumption the rest of this file rests on: the message that opened the turn is
// bound to a thread the delivery can answer in.
func TestTheThreadBindingIsReadableForDelivery(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	integration, err := connectSlack(t, placements, organization, "Slack — Acme",
		slackInstallation("T0ACME"))
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}
	outcome, err := placements.RecordSlackMessage(context.Background(), organization,
		storage.SlackMessage{
			Integration: integration.ID, BodyDigest: randomDigest(t),
			Channel: "C0INCIDENTS", Thread: "1700000001.1",
			Subject: "why?", ActorID: "U9SRE", ActorDisplay: "U9SRE", Text: "why?",
		})
	if err != nil {
		t.Fatalf("recording a slack message: %v", err)
	}

	channel, thread, through, bound, err := placements.SlackThreadOf(context.Background(),
		organization, outcome.Conversation)
	if err != nil || !bound {
		t.Fatalf("reading the binding = %v, bound=%v", err, bound)
	}
	if channel != "C0INCIDENTS" || thread != "1700000001.1" || through != integration.ID {
		t.Errorf("the binding answers %s/%s through %s", channel, thread, through)
	}
}

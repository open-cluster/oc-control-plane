package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func aSlackTurn(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	workspace, channel, thread string,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	integration, err := connectSlack(t, database, organization,
		"Slack — "+workspace, slackInstallation(workspace))
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	outcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration:   integration.ID,
		ContentDigest: randomDigest(t),
		Channel:       channel,
		Thread:        thread,
		Subject:       "why is checkout failing?",
		ActorID:       "U9SRE",
		ActorDisplay:  "U9SRE",
		Text:          "why is checkout failing?",
	})
	if err != nil {
		t.Fatalf("recording a slack message: %v", err)
	}

	turn, opened, err := database.OpenTurn(ctx, organization, outcome.Conversation, time.Hour)
	if err != nil {
		t.Fatalf("opening a turn: %v", err)
	}
	if !opened {
		t.Fatal("the first message of a conversation opened no turn")
	}
	return turn.InvestigationID, integration.ID
}

func claimed(
	t *testing.T, database *storage.Database, investigation uuid.UUID, lease time.Duration,
) slack.Reply {
	t.Helper()

	held, err := database.ClaimSlackReplies(context.Background(), 10, lease)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range held {
		if one.Investigation == investigation {
			return one
		}
	}
	t.Fatalf("the delivery for %s was not claimed: %+v", investigation, held)
	return slack.Reply{}
}

func TestEveryTurnOfASlackConversationOwesAnAnswer(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	investigation, integration := aSlackTurn(t, database, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")

	claimed, err := database.ClaimSlackReplies(context.Background(), 10, time.Minute)
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
	if one.Stream.Channel != "C0INCIDENTS" || one.Stream.Thread != "1700000001.1" {
		t.Errorf("the delivery answers into %s/%s, want the thread the question was asked in",
			one.Stream.Channel, one.Stream.Thread)
	}
	if one.Stream.TS != "" || one.LastSequence != 0 {
		t.Errorf("a fresh delivery already claims progress: %+v", one)
	}
}

func TestSlackReplyCannotBeRetargetedBetweenAttempts(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	investigation, integration := aSlackTurn(t, database, organization, "T-ORIGINAL", "C-ORIGINAL", "1700000001.1")
	reply := claimed(t, database, investigation, time.Minute)
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_conversation SET channel_id='C-OTHER'
WHERE org_id=$1 AND conversation_id=$2`, organization, reply.Conversation); err == nil {
		t.Fatal("pending reply destination was retargeted")
	}
	if err := database.RetrySlackReply(ctx, organization, investigation, reply.ClaimToken,
		time.Now().Add(-time.Second), "retry", false); err != nil {
		t.Fatal(err)
	}
	retried := claimed(t, database, investigation, time.Minute)
	if retried.Integration != integration || retried.Stream.Channel != "C-ORIGINAL" || retried.Stream.Thread != "1700000001.1" {
		t.Fatalf("retry changed destination: %+v", retried)
	}
	if err := database.CompleteSlackReply(ctx, organization, investigation, retried.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteIntegration(ctx, ownerOf(t, organization), organization, integration); !errors.Is(err, integrations.ErrInUse) {
		t.Fatalf("outstanding Webhook Job did not prevent disconnection: %v", err)
	}
	if _, _, _, _, found, err := database.SlackReplyState(ctx, organization, investigation); err != nil || !found {
		t.Fatalf("refused disconnection removed reply state: found=%v, %v", found, err)
	}
}

func TestSlackConversationRetainsItsExactOriginatingThread(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	integration, err := connectSlack(t, database, organization,
		"Slack — originating workspace", slackInstallation("T-ORIGIN"))
	if err != nil {
		t.Fatalf("connecting Slack: %v", err)
	}
	outcome, err := database.RecordSlackMessage(context.Background(), organization,
		storage.SlackMessage{
			Integration: integration.ID, ContentDigest: randomDigest(t),
			Channel: "C-INCIDENT", Thread: "1710000000.1", MessageID: "1710000000.2",
			Subject: "checkout latency", ActorID: "U-SRE", ActorDisplay: "On-call",
			Text: "why is checkout slow?",
		})
	if err != nil {
		t.Fatalf("recording the originating app mention: %v", err)
	}

	origin, err := database.ConversationOrigin(context.Background(), organization,
		outcome.Conversation)
	if err != nil {
		t.Fatalf("reading the Conversation orientation: %v", err)
	}
	if origin == nil || origin.IntegrationID != integration.ID ||
		origin.Channel != "C-INCIDENT" || origin.Thread != "1710000000.1" {
		t.Fatalf("Conversation origin was not structurally retained: %+v", origin)
	}
}

func TestSlackMessageCannotBindAnotherOrganizationsIntegration(t *testing.T) {
	database, first := migratedDatabase(t)
	second := organization(t, "org-second")
	integration, err := connectSlack(t, database, first,
		"Slack — first", slackInstallation("T-FIRST"))
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}

	_, err = database.RecordSlackMessage(context.Background(), second, storage.SlackMessage{
		Integration:   integration.ID,
		ContentDigest: randomDigest(t),
		Channel:       "C-SECOND",
		Thread:        "1700000002.1",
		Subject:       "must not cross the organization boundary",
		ActorID:       "U-SECOND",
		Text:          "investigate",
	})
	if err == nil {
		t.Fatal("another Organization's Integration was accepted")
	}

	pool, poolErr := database.Pool(second)
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	var conversations int
	if queryErr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversation WHERE org_id = $1`, second.String()).
		Scan(&conversations); queryErr != nil {
		t.Fatal(queryErr)
	}
	if conversations != 0 {
		t.Fatalf("another Organization's Integration opened %d conversations", conversations)
	}
}

func TestAConversationOutsideSlackOwesNothing(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	ctx := context.Background()
	opened, err := database.OpenConversation(ctx, ownerOf(t, organization), organization,
		conversation.NewConversation{
			Surface: conversation.SurfaceWeb, Subject: "asked in the console",
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a console conversation: %v", err)
	}
	if _, err := database.AppendMessage(ctx, ownerOf(t, organization), organization,
		opened.ID, conversation.NewMessage{
			Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: "user-under-test", Text: "why is checkout failing?",
		}); err != nil {
		t.Fatalf("saying something: %v", err)
	}
	if _, _, err := database.OpenTurn(ctx, organization, opened.ID, time.Hour); err != nil {
		t.Fatalf("opening its turn: %v", err)
	}

	claimed, err := database.ClaimSlackReplies(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("a console conversation owes a slack answer: %+v", claimed)
	}
}

func TestAClaimedDeliveryIsNotClaimedTwice(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	aSlackTurn(t, database, organization, "T0ACME", "C0INCIDENTS", "1700000001.1")

	first, err := database.ClaimSlackReplies(context.Background(), 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("the first claim = %+v, %v", first, err)
	}
	second, err := database.ClaimSlackReplies(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a leased delivery was claimed again: %+v", second)
	}
}

func TestTheCursorOnlyEverMovesForward(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	investigation, _ := aSlackTurn(t, database, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	reply := claimed(t, database, investigation, time.Minute)

	if err := database.AdvanceSlackReply(ctx, organization, investigation, reply.ClaimToken,
		slack.Progress{Stream: slack.Stream{TS: "1700000100.100", Native: true}, Sequence: 12}); err != nil {
		t.Fatalf("advancing: %v", err)
	}
	if err := database.AdvanceSlackReply(ctx, organization, investigation, reply.ClaimToken,
		slack.Progress{Stream: slack.Stream{TS: "1700000100.100", Native: true}, Sequence: 4}); err != nil {
		t.Fatalf("advancing backwards: %v", err)
	}

	_, sequence, streamTS, _, found, err := database.SlackReplyState(ctx,
		organization, investigation)
	if err != nil || !found {
		t.Fatalf("reading the delivery = %v, found=%v", err, found)
	}
	if sequence != 12 {
		t.Errorf("the cursor is at %d, want it to have stayed at 12", sequence)
	}
	if err := database.AdvanceSlackReply(ctx, organization, investigation, reply.ClaimToken,
		slack.Progress{Stream: slack.Stream{TS: "1700000999.999", Native: true}, Sequence: 13}); err != nil {
		t.Fatalf("advancing: %v", err)
	}
	_, _, again, _, _, err := database.SlackReplyState(ctx, organization, investigation)
	if err != nil {
		t.Fatalf("reading the delivery: %v", err)
	}
	if again != streamTS {
		t.Errorf("the visible message moved from %q to %q", streamTS, again)
	}
}

func TestGivingUpEndsTheDeliveryAndNotTheInvestigation(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	investigation, _ := aSlackTurn(t, database, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	reply := claimed(t, database, investigation, time.Minute)

	if err := database.RetrySlackReply(ctx, organization, investigation, reply.ClaimToken,
		time.Now(), "slack would not open the reply", true); err != nil {
		t.Fatalf("giving up: %v", err)
	}

	status, _, _, note, found, err := database.SlackReplyState(ctx, organization, investigation)
	if err != nil || !found {
		t.Fatalf("reading the delivery = %v, found=%v", err, found)
	}
	if status != storage.SlackReplyFailed {
		t.Errorf("status = %d, want failed", status)
	}
	if note == "" {
		t.Error("giving up recorded no reason an operator could read")
	}
	claimed, err := database.ClaimSlackReplies(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range claimed {
		if one.Investigation == investigation {
			t.Error("a delivery that gave up was claimed again")
		}
	}

	record, err := database.Investigation(ctx, organization, investigation)
	if err != nil {
		t.Fatalf("reading the investigation: %v", err)
	}
	if record.ID != investigation {
		t.Errorf("the investigation record is not readable after a failed delivery")
	}
}

func TestADeliveredAnswerIsNeverClaimedAgain(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	investigation, _ := aSlackTurn(t, database, organization,
		"T0ACME", "C0INCIDENTS", "1700000001.1")
	ctx := context.Background()
	reply := claimed(t, database, investigation, time.Minute)

	if err := database.CompleteSlackReply(ctx, organization, investigation, reply.ClaimToken); err != nil {
		t.Fatalf("completing: %v", err)
	}
	claimed, err := database.ClaimSlackReplies(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	for _, one := range claimed {
		if one.Investigation == investigation {
			t.Error("a delivered answer was claimed again, which would repeat it")
		}
	}
}

func TestTheThreadBindingIsReadableForDelivery(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	integration, err := connectSlack(t, database, organization, "Slack — Acme",
		slackInstallation("T0ACME"))
	if err != nil {
		t.Fatalf("connecting slack: %v", err)
	}
	outcome, err := database.RecordSlackMessage(context.Background(), organization,
		storage.SlackMessage{
			Integration: integration.ID, ContentDigest: randomDigest(t),
			Channel: "C0INCIDENTS", Thread: "1700000001.1",
			Subject: "why?", ActorID: "U9SRE", ActorDisplay: "U9SRE", Text: "why?",
		})
	if err != nil {
		t.Fatalf("recording a slack message: %v", err)
	}

	channel, thread, through, bound, err := database.SlackThreadOf(context.Background(),
		organization, outcome.Conversation)
	if err != nil || !bound {
		t.Fatalf("reading the binding = %v, bound=%v", err, bound)
	}
	if channel != "C0INCIDENTS" || thread != "1700000001.1" || through != integration.ID {
		t.Errorf("the binding answers %s/%s through %s", channel, thread, through)
	}
}

func TestExpiredSlackClaimCannotAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id, _ := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	reply := claimed(t, database, id, -time.Second)
	err := database.AdvanceSlackReply(ctx, org, id, reply.ClaimToken, slack.Progress{
		Stream: slack.Stream{TS: "stale-message", Native: true}, Sequence: 9,
	})
	if err == nil {
		t.Fatal("an expired worker advanced the reply")
	}
	_, sequence, message, _, found, err := database.SlackReplyState(ctx, org, id)
	if err != nil || !found || sequence != 0 || message != "" {
		t.Fatalf("stale write changed delivery: sequence=%d message=%q found=%v err=%v", sequence, message, found, err)
	}
}

func TestReclaimedSlackReplyRejectsPreviousGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id, _ := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	old := claimed(t, database, id, -time.Second)
	current := claimed(t, database, id, time.Minute)
	if old.ClaimToken == uuid.Nil || current.ClaimToken == uuid.Nil || old.ClaimToken == current.ClaimToken {
		t.Fatal("each claim must have a distinct nonzero token")
	}
	for _, token := range []uuid.UUID{old.ClaimToken, uuid.Nil, uuid.New()} {
		for _, write := range []func() error{
			func() error {
				return database.AdvanceSlackReply(ctx, org, id, token, slack.Progress{Stream: slack.Stream{TS: "stale"}, Sequence: 99})
			},
			func() error { return database.RetrySlackReply(ctx, org, id, token, time.Now(), "stale", true) },
			func() error { return database.CompleteSlackReply(ctx, org, id, token) },
			func() error { return database.ReleaseSlackReply(ctx, org, id, token, time.Now()) },
		} {
			if err := write(); !errors.Is(err, slack.ErrReplyClaimLost) {
				t.Fatalf("stale mutation returned %v", err)
			}
		}
	}
	if err := database.AdvanceSlackReply(ctx, org, id, current.ClaimToken, slack.Progress{Stream: slack.Stream{TS: "current"}, Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteSlackReply(ctx, org, id, current.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := database.RetrySlackReply(ctx, org, id, current.ClaimToken, time.Now(), "late retry", false); !errors.Is(err, slack.ErrReplyClaimLost) {
		t.Fatalf("completed delivery accepted a retry: %v", err)
	}
	_, sequence, message, note, found, err := database.SlackReplyState(ctx, org, id)
	if err != nil || !found || sequence != 2 || message != "current" || note != "" {
		t.Fatalf("delivery changed: sequence=%d message=%q note=%q found=%v err=%v", sequence, message, note, found, err)
	}
}

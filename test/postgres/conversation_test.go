package storage_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const turnWindowLead = time.Hour

func twoOrganizationsInOneDatabase(
	t *testing.T,
) (*storage.Database, uuid.UUID, uuid.UUID) {
	t.Helper()

	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	first, second := organization(t, "org-a"), organization(t, "org-b")
	ensureTestOrganization(t, database, first)
	ensureTestOrganization(t, database, second)
	return database, first, second
}

func conclusionSaying(answer string) investigation.Conclusion {
	return investigation.Conclusion{Summary: answer}
}

func openConversation(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	subject string,
) conversation.Conversation {
	t.Helper()

	opened, err := database.OpenConversation(context.Background(),
		ownerOf(t, organization), organization, conversation.NewConversation{
			Source: conversation.SourceWeb, Title: subject,
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a conversation: %v", err)
	}
	return opened
}

func say(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	id uuid.UUID, text string,
) conversation.Message {
	t.Helper()

	said, err := appendMessageForTest(database, context.Background(),
		ownerOf(t, organization), organization, id, conversation.NewMessage{
			Role:     conversation.RoleUser,
			AuthorID: "user-under-test", AuthorName: "Test Operator", Text: text,
		})
	if err != nil {
		t.Fatalf("appending a message: %v", err)
	}
	return said
}

func TestAnotherOrganizationsConversationIsNotFound(t *testing.T) {
	t.Parallel()

	database, mine, theirs := twoOrganizationsInOneDatabase(t)
	opened := openConversation(t, database, mine, "checkout is slow")

	if _, err := database.ConversationDetail(context.Background(), theirs, opened.ID,
		50); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("reading the detail across tenants answered %v", err)
	}
	if _, _, err := openTurnForTest(database, context.Background(), theirs, opened.ID,
		turnWindowLead); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("opening a turn across tenants answered %v", err)
	}
	if _, err := appendMessageForTest(database, context.Background(), ownerOf(t, theirs), theirs,
		opened.ID, conversation.NewMessage{
			Role:     conversation.RoleUser,
			AuthorID: "somebody-else", Text: "what is this about?",
		}); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("saying something into another tenant's conversation answered %v", err)
	}
}

func TestTheConversationListingNarrowsServerSide(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	checkout := openConversation(t, database, organization, "checkout is slow")
	openConversation(t, database, organization, "payments are failing")
	openConversation(t, database, organization, "checkout returns 500")

	listed, err := database.QueryConversations(context.Background(),
		ownerOf(t, organization), organization, conversation.Page{Search: "CHECKOUT"})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(listed.Conversations) != 2 {
		t.Errorf("%d conversations matched %q, want 2", len(listed.Conversations),
			"CHECKOUT")
	}

	listed, err = database.QueryConversations(context.Background(),
		ownerOf(t, organization), organization,
		conversation.Page{Search: "checkout", Limit: 1})
	if err != nil {
		t.Fatalf("searching one page: %v", err)
	}
	if len(listed.Conversations) != 1 || listed.Next == "" {
		t.Fatalf("page one = %d conversations, next=%q", len(listed.Conversations),
			listed.Next)
	}
	first := listed.Conversations[0].ID
	listed, err = database.QueryConversations(context.Background(),
		ownerOf(t, organization), organization,
		conversation.Page{Search: "checkout", Limit: 1, After: listed.Next})
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if len(listed.Conversations) != 1 || listed.Conversations[0].ID == first {
		t.Errorf("page two = %+v; a cursor must resume the narrowed order",
			listed.Conversations)
	}
	if listed.Conversations[0].ID != checkout.ID && first != checkout.ID {
		t.Errorf("neither page carried the conversation the search was for")
	}
}

func TestConversationListingAppliesAscendingSortAcrossPages(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	first := openConversation(t, database, organization, "first")
	second := openConversation(t, database, organization, "second")

	page, err := database.QueryConversations(context.Background(), ownerOf(t, organization),
		organization, conversation.Page{Limit: 1, Sort: "lastActivityAt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ID != first.ID || page.Next == "" {
		t.Fatalf("first page = %+v", page)
	}
	if _, err = database.QueryConversations(context.Background(), ownerOf(t, organization),
		organization, conversation.Page{
			Limit: 1, Sort: "lastActivityAt", Descending: true, After: page.Next,
		}); !errors.Is(err, conversation.ErrBadCursor) {
		t.Fatalf("cursor reused with another order = %v, want ErrBadCursor", err)
	}
	page, err = database.QueryConversations(context.Background(), ownerOf(t, organization),
		organization, conversation.Page{Limit: 1, Sort: "lastActivityAt", After: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ID != second.ID {
		t.Fatalf("second page = %+v", page)
	}
}

func TestFreshSchemaOmitsTheRetiredSamplerIndex(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	err = pool.QueryRow(context.Background(), `SELECT EXISTS (
		SELECT 1 FROM pg_indexes WHERE schemaname='public'
		AND indexname='conversation_message_person_history_idx')`).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("retired fixed-position sampler index still exists")
	}
}

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
		Integration: integration.ID, ContentDigest: randomDigest(t), Channel: "CORIGIN", Thread: "1.0",
		Subject: "origin", AuthorID: "UORIGIN", Text: "question",
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
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	_, took, err := openTurnForTest(database, ctx, org, chat.Conversation, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening Slack turn: took=%v error=%v", took, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE conversation_message RENAME TO unavailable_history`); err != nil {
		t.Fatal(err)
	}
	independent, err := database.ConversationOrigin(ctx, org, chat.Conversation)
	if err != nil || independent == nil || *independent != *origin {
		t.Fatalf("history failure changed origin: %+v err=%v", independent, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE unavailable_history RENAME TO conversation_message`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM slack_conversation WHERE org_id = $1 AND conversation_id = $2`,
		org, chat.Conversation); err != nil {
		t.Fatal(err)
	}
	if origin, err := database.ConversationOrigin(ctx, org, chat.Conversation); err == nil {
		t.Fatalf("missing provider binding became unrestricted origin: %+v", origin)
	}
}

func TestAuthenticatedAppendRefusesSlackConversationWithoutSideEffects(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	integration, err := connectSlack(t, database, org, "Slack", slackInstallation("TREFUSE"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := database.RecordSlackMessage(ctx, org, storage.SlackMessage{
		Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CREFUSE", Thread: "1.0", Subject: "Slack only",
		AuthorID: "USLACK", AuthorName: "Slack User", Text: "first",
	})
	if err != nil {
		t.Fatal(err)
	}

	principal := ownerOf(t, org)
	_, err = database.AdmitAgentChat(ctx, principal, org, recorded.Conversation, uuid.New(),
		conversation.NewMessage{Text: "must not cross sources"})
	if !errors.Is(err, conversation.ErrUnknown) {
		t.Fatalf("authenticated append error = %v, want source mismatch", err)
	}

	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	var messages, turns, audits int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM conversation_message WHERE org_id = $1 AND conversation_id = $2),
		(SELECT count(*) FROM investigation WHERE org_id = $1 AND conversation_id = $2),
		(SELECT count(*) FROM audit_event WHERE org_id = $1 AND action = 'conversation.message-sent' AND target_id = $2::text)`,
		org, recorded.Conversation).Scan(&messages, &turns, &audits); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || turns != 0 || audits != 0 {
		t.Fatalf("refused append persisted messages=%d turns=%d audits=%d", messages, turns, audits)
	}
}

func TestSlackQueueCapacityPreservesDeduplicationAndRetry(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	integration, err := connectSlack(t, database, org, "Slack", slackInstallation("TQUEUE"))
	if err != nil {
		t.Fatal(err)
	}
	said := storage.SlackMessage{Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CQUEUE", Thread: "1.0", Subject: "queue", AuthorID: "UQUEUE", Text: "first"}
	slack, err := database.RecordSlackMessage(ctx, org, said)
	if err != nil {
		t.Fatal(err)
	}
	chat := openConversation(t, database, org, "web backlog")
	principal := ownerOf(t, org)
	for range 99 {
		if _, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
			conversation.NewMessage{Text: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	if outcome, err := database.RecordSlackMessage(ctx, org, said); err != nil || !outcome.Duplicate {
		t.Fatalf("duplicate at capacity: %+v, %v", outcome, err)
	}
	said.ContentDigest = randomDigest(t)
	said.Text = "retry after capacity is available"
	if _, err := database.RecordSlackMessage(ctx, org, said); !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("new Message at capacity: %v", err)
	}
	if _, started, err := openTurnForTest(database, ctx, org, slack.Conversation, turnWindowLead); err != nil || !started {
		t.Fatalf("drain: %v, %v", started, err)
	}
	if outcome, err := database.RecordSlackMessage(ctx, org, said); err != nil || outcome.Duplicate {
		t.Fatalf("retry after refusal: %+v, %v", outcome, err)
	}
}

func TestAtomicMessageAppendRollsBackDatabaseFailures(t *testing.T) {
	for _, table := range []string{"audit_event", "agent_execution"} {
		t.Run(table, func(t *testing.T) {
			database, org, _ := twoOrganizationsInOneDatabase(t)
			ctx := context.Background()
			chat := openConversation(t, database, org, "rollback")
			pool, err := poolForTest(database, org)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_queue_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'injected failure'; END $$;
				CREATE TRIGGER reject_queue_write BEFORE INSERT ON `+table+`
				FOR EACH ROW EXECUTE FUNCTION reject_queue_write()`); err != nil {
				t.Fatal(err)
			}
			_, err = database.AdmitAgentChat(ctx, ownerOf(t, org), org, chat.ID, uuid.New(),
				conversation.NewMessage{Text: "must roll back"})
			if err == nil {
				t.Fatal("injected database failure was reported as acceptance")
			}
			detail, err := database.ConversationDetail(ctx, org, chat.ID, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.Messages) != 0 || len(detail.Turns) != 0 {
				t.Fatalf("partial transaction persisted: %+v", detail)
			}
		})
	}
}

func TestQueuedMessageAdmissionIsAtomicAcrossConversations(t *testing.T) {
	database, org, other := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	chats := []conversation.Conversation{
		openConversation(t, database, org, "first"), openConversation(t, database, org, "second"),
	}
	principal := ownerOf(t, org)
	for i := range 99 {
		if _, err := database.AdmitAgentChat(ctx, principal, org, chats[i%2].ID, uuid.New(),
			conversation.NewMessage{Text: fmt.Sprintf("queued %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, chat := range chats {
		go func() {
			<-start
			_, err := database.AdmitAgentChat(ctx, principal, org, chat.ID, uuid.New(),
				conversation.NewMessage{Text: "last available slot"})
			results <- err
		}()
	}
	close(start)
	accepted, refused := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			accepted++
		case errors.Is(err, conversation.ErrQueueFull):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d; want one of each", accepted, refused)
	}
	otherChat := openConversation(t, database, other, "unaffected Organization")
	if _, err := database.AdmitAgentChat(ctx, ownerOf(t, other), other, otherChat.ID, uuid.New(),
		conversation.NewMessage{Text: "still accepted"}); err != nil {
		t.Fatal(err)
	}
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message
		WHERE org_id = $1 AND client_message_id IS NOT NULL AND role = 1`, org).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 100 {
		t.Fatalf("persisted queued Messages = %d, want 100", queued)
	}
}

func TestConversationDetailBoundsTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org, otherOrg := twoOrganizationsInOneDatabase(t)
	opened := openConversation(t, database, org, "long conversation")
	for range 201 {
		say(t, database, org, opened.ID, "continue")
		turn, took, err := openTurnForTest(database, ctx, org, opened.ID, turnWindowLead)
		if err != nil || !took {
			t.Fatalf("opening turn: took=%v err=%v", took, err)
		}
		if err = seedInvestigationConclusion(ctx, database, org, turn.InvestigationID, conclusionSaying("answer")); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := database.ConversationDetail(ctx, org, opened.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Turns) != 50 || detail.Turns[0].Ordinal != 1 || detail.Turns[49].Ordinal != 50 {
		t.Fatalf("detail returned %d turns; want first 50 in ascending order", len(detail.Turns))
	}
	if detail.TurnsNext == "" {
		t.Fatal("bounded detail omitted its continuation")
	}
	page, err := database.ConversationTurns(ctx, org, opened.ID, 0, detail.TurnsNext)
	if err != nil || len(page.Turns) != 50 || page.Turns[0].Ordinal != 51 {
		t.Fatalf("detail continuation = %+v err=%v", page, err)
	}
	page, err = database.ConversationTurns(ctx, org, opened.ID, 999, "")
	if err != nil || len(page.Turns) != 200 || page.Next == "" {
		t.Fatalf("maximum page returned %d turns, next=%q err=%v", len(page.Turns), page.Next, err)
	}
	page, err = database.ConversationTurns(ctx, org, opened.ID, 200, page.Next)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].Ordinal != 201 || page.Next != "" {
		t.Fatalf("last page = %+v err=%v", page, err)
	}
	other := openConversation(t, database, otherOrg, "another organization")
	if _, err = database.ConversationTurns(ctx, otherOrg, other.ID, 50, detail.TurnsNext); !errors.Is(err, conversation.ErrBadCursor) {
		t.Fatalf("cursor reused across Organizations: %v", err)
	}
	for _, id := range []uuid.UUID{opened.ID, uuid.New()} {
		if _, err = database.ConversationTurns(ctx, otherOrg, id, 50, ""); !errors.Is(err, conversation.ErrUnknown) {
			t.Fatalf("foreign or missing Conversation: %v", err)
		}
	}
}

func TestInvestigationMessagesPreserveOnlyTheirAssignedBatch(t *testing.T) {
	database, org, other := twoOrganizationsInOneDatabase(t)
	chat := openConversation(t, database, org, "assigned input")
	ctx := context.Background()
	var want []investigation.AssignedMessage
	for n := range 15 {
		message := say(t, database, org, chat.ID, strings.Repeat("界", 1100)+fmt.Sprintf(" correction-%d", n))
		want = append(want, investigation.AssignedMessage{Sequence: message.Sequence, Author: message.AuthorName,
			CreatedAt: message.CreatedAt, Text: message.Text})
	}
	turn, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead)
	if err != nil || !started {
		t.Fatalf("opening turn: %v %v", started, err)
	}
	say(t, database, org, chat.ID, "later queued request must not become current input")
	got, err := database.InvestigationMessages(ctx, org, chat.ID, turn.InvestigationID)
	if err != nil || len(got) != len(want) {
		t.Fatalf("assigned Messages: count=%d err=%v", len(got), err)
	}
	for n := range want {
		if got[n].Sequence != want[n].Sequence || got[n].Author != want[n].Author ||
			!got[n].CreatedAt.Equal(want[n].CreatedAt) || got[n].Text != want[n].Text {
			t.Fatalf("assigned Message %d lost ordering, attribution, time or text", n)
		}
	}
	if _, err := database.InvestigationMessages(ctx, other, chat.ID, turn.InvestigationID); err == nil {
		t.Fatal("another Organization read assigned input")
	}
	sibling := openConversation(t, database, org, "other Conversation")
	if _, err := database.InvestigationMessages(ctx, org, sibling.ID, turn.InvestigationID); err == nil {
		t.Fatal("another Conversation read assigned input")
	}
}

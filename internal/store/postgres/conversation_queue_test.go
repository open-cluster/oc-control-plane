package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestAtomicAppendQueuesWhileTurnIsActive(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	opened := openConversation(t, database, org, "queued follow-up")
	appendPerson := func(text string) (conversation.Message, conversation.Turn, bool, error) {
		return database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, opened.ID,
			conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
				ActorID: "user-under-test", ActorDisplay: "Test Operator", Text: text}, turnWindowLead, 100)
	}
	_, first, started, err := appendPerson("initial question")
	if err != nil || !started {
		t.Fatalf("initial append: started=%v, err=%v", started, err)
	}
	queued, _, started, err := appendPerson("correction while running")
	if err != nil || started {
		t.Fatalf("follow-up append: started=%v, err=%v", started, err)
	}
	if !queued.Queued() || queued.Sequence != 2 {
		t.Fatalf("follow-up = %+v", queued)
	}
	if err := database.ConcludeInvestigation(ctx, org, first.InvestigationID,
		conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	second, started, err := database.OpenTurn(ctx, org, opened.ID, turnWindowLead)
	if err != nil || !started || second.Ordinal != 2 {
		t.Fatalf("drain: turn=%+v, started=%v, err=%v", second, started, err)
	}
	detail, err := database.ConversationDetail(ctx, org, opened.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.Messages[1].InvestigationID != second.InvestigationID {
		t.Fatalf("persisted Messages = %+v", detail.Messages)
	}
}

func TestCompetingAtomicAppendsDrainExactlyOnce(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	chat := openConversation(t, database, org, "competing follow-ups")
	principal := ownerOf(t, org)
	appendPerson := func(text string) (conversation.Turn, bool, error) {
		_, turn, started, err := database.AppendMessageAndOpenTurn(ctx, principal, org, chat.ID,
			conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
				ActorID: principal.ID(), Text: text}, turnWindowLead, 100)
		return turn, started, err
	}
	first, started, err := appendPerson("initial question")
	if err != nil || !started {
		t.Fatalf("initial turn: %v, %v", started, err)
	}
	start := make(chan struct{})
	appended := make(chan error, 10)
	for i := range 10 {
		go func() {
			<-start
			_, opened, err := appendPerson(fmt.Sprintf("follow-up %d", i))
			if err == nil && opened {
				err = errors.New("opened another active turn")
			}
			appended <- err
		}()
	}
	close(start)
	for range 10 {
		if err := <-appended; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.ConcludeInvestigation(ctx, org, first.InvestigationID,
		conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	type drainResult struct {
		turn    conversation.Turn
		started bool
		err     error
	}
	drained := make(chan drainResult, 2)
	for range 2 {
		go func() {
			turn, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead)
			drained <- drainResult{turn, started, err}
		}()
	}
	var second conversation.Turn
	opened := 0
	for range 2 {
		result := <-drained
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.started {
			opened++
			second = result.turn
		}
	}
	detail, err := database.ConversationDetail(ctx, org, chat.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || len(detail.Turns) != 2 || len(detail.Messages) != 11 {
		t.Fatalf("drains=%d turns=%d Messages=%d", opened, len(detail.Turns), len(detail.Messages))
	}
	for _, message := range detail.Messages[1:] {
		if message.InvestigationID != second.InvestigationID {
			t.Fatalf("follow-up not assigned to the one next turn: %+v", message)
		}
	}
}

func TestSlackQueueCapacityPreservesDeduplicationAndRetry(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	integration, err := connectSlack(t, database, org, "Slack", slackInstallation("TQUEUE"))
	if err != nil {
		t.Fatal(err)
	}
	said := storage.SlackMessage{Integration: integration.ID, BodyDigest: randomDigest(t),
		Channel: "CQUEUE", Thread: "1.0", Subject: "queue", ActorID: "UQUEUE", Text: "first"}
	if _, err := database.RecordSlackMessage(ctx, org, said); err != nil {
		t.Fatal(err)
	}
	chat := openConversation(t, database, org, "web backlog")
	for range 99 {
		say(t, database, org, chat.ID, "queued")
	}
	if outcome, err := database.RecordSlackMessage(ctx, org, said); err != nil || !outcome.Duplicate {
		t.Fatalf("duplicate at capacity: %+v, %v", outcome, err)
	}
	said.BodyDigest = randomDigest(t)
	said.Text = "retry after capacity is available"
	if _, err := database.RecordSlackMessage(ctx, org, said); !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("new Message at capacity: %v", err)
	}
	if _, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead); err != nil || !started {
		t.Fatalf("drain: %v, %v", started, err)
	}
	if outcome, err := database.RecordSlackMessage(ctx, org, said); err != nil || outcome.Duplicate {
		t.Fatalf("retry after refusal: %+v, %v", outcome, err)
	}
}

func TestAtomicMessageAppendRollsBackDatabaseFailures(t *testing.T) {
	for _, table := range []string{"audit_event", "investigation"} {
		t.Run(table, func(t *testing.T) {
			database, org, _ := twoOrganizationsInOneDatabase(t)
			ctx := context.Background()
			chat := openConversation(t, database, org, "rollback")
			pool, err := database.Pool(org)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_queue_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'injected failure'; END $$;
				CREATE TRIGGER reject_queue_write BEFORE INSERT ON `+table+`
				FOR EACH ROW EXECUTE FUNCTION reject_queue_write()`); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, chat.ID,
				conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
					ActorID: "user-under-test", Text: "must roll back"}, turnWindowLead, 100)
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
	for _, chat := range chats {
		say(t, database, org, chat.ID, "initial question")
		if _, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead); err != nil || !started {
			t.Fatalf("start: %v, %v", started, err)
		}
	}
	for i := range 99 {
		say(t, database, org, chats[i%2].ID, fmt.Sprintf("queued %d", i))
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, chat := range chats {
		go func() {
			<-start
			_, _, _, err := database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, chat.ID,
				conversation.NewMessage{Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
					ActorID: "user-under-test", Text: "last available slot"}, turnWindowLead, 100)
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
	say(t, database, other, otherChat.ID, "still accepted")
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message
		WHERE org_id = $1 AND investigation_id IS NULL AND role = 1`, org.String()).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 100 {
		t.Fatalf("persisted queued Messages = %d, want 100", queued)
	}
}

func TestLegacyMessageBacklogDrainsInBoundedOrderedBatches(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	chat := openConversation(t, database, org, "legacy backlog")
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_message
		(conversation_id, org_id, sequence, role, actor_kind, actor_id, actor_display, text)
		SELECT $1, $2, n, 1, 1, 'actor-' || n, 'Operator', 'question-' || n
		FROM generate_series(1, 205) n`, chat.ID, org.String()); err != nil {
		t.Fatal(err)
	}
	for batch, want := range []int{100, 100, 5} {
		turn, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead)
		if err != nil || !started {
			t.Fatalf("batch %d: started=%v, err=%v", batch, started, err)
		}
		var count, first, last int
		if err := pool.QueryRow(ctx, `SELECT count(*), min(sequence), max(sequence)
			FROM conversation_message WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3`,
			org.String(), chat.ID, turn.InvestigationID).Scan(&count, &first, &last); err != nil {
			t.Fatal(err)
		}
		if count != want || first != batch*100+1 || last != batch*100+want {
			t.Fatalf("batch %d: count=%d first=%d last=%d", batch, count, first, last)
		}
		var actor string
		if err := pool.QueryRow(ctx, `SELECT created_by FROM investigation
			WHERE org_id = $1 AND investigation_id = $2`, org.String(), turn.InvestigationID).Scan(&actor); err != nil {
			t.Fatal(err)
		}
		if actor != fmt.Sprintf("actor-%d", last) {
			t.Fatalf("batch attributed to %q, want actor-%d", actor, last)
		}
		if err := database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
			conclusionSaying("answer"), "", investigation.Usage{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, started, err := database.OpenTurn(ctx, org, chat.ID, turnWindowLead); err != nil || started {
		t.Fatalf("empty drain: started=%v, err=%v", started, err)
	}
}

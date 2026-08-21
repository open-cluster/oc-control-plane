package storage_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Conversations at the storage seam. What is asserted here is what the DATABASE keeps on
// its own — the sequence, the single-writer invariant, the queue and its drain — because
// those are the properties that have to hold across replicas, and a test that went through
// one process could not tell whether they do.

const turnWindowLead = time.Hour

// twoOrganizationsOnOnePlacement is the shape the tenant boundary has to hold in: one
// database, two tenants, the organization a column rather than a connection. Two separate
// placements would prove nothing — the pools could not reach each other's rows anyway.
func twoOrganizationsOnOnePlacement(
	t *testing.T,
) (*storage.Placements, tenancy.Organization, tenancy.Organization) {
	t.Helper()

	placements := openPlacements(t,
		map[string]string{"shared": postgresDSN(t)},
		map[string]string{"org-a": "shared", "org-b": "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return placements, organization(t, "org-a"), organization(t, "org-b")
}

// conclusionSaying is a minimal concluding document: an answer and no findings, which is
// legal — a turn that established nothing still concluded.
func conclusionSaying(answer string) investigation.Conclusion {
	return investigation.Conclusion{Answer: answer}
}

// openConversation records one for a test, with no episode.
func openConversation(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	subject string,
) conversation.Conversation {
	t.Helper()

	opened, err := placements.OpenConversation(context.Background(),
		ownerOf(t, organization), organization, conversation.NewConversation{
			Surface: conversation.SurfaceWeb, Subject: subject,
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a conversation: %v", err)
	}
	return opened
}

// say appends one person message.
func say(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	id uuid.UUID, text string,
) conversation.Message {
	t.Helper()

	said, err := placements.AppendMessage(context.Background(),
		ownerOf(t, organization), organization, id, conversation.NewMessage{
			Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: "user-under-test", ActorDisplay: "Test Operator", Text: text,
		})
	if err != nil {
		t.Fatalf("appending a message: %v", err)
	}
	return said
}

// The sequence is assigned by the database, from one, without gaps. It is what the whole
// transcript is ordered by, and a client that reconnects asks for what comes after a
// number it already has.
func TestAConversationsMessagesTakeConsecutiveSequences(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")

	for position, text := range []string{"what changed?", "ignore the database",
		"check deployments instead"} {
		said := say(t, placements, organization, opened.ID, text)
		if said.Sequence != int64(position+1) {
			t.Errorf("message %d took sequence %d, want %d", position, said.Sequence,
				position+1)
		}
	}
}

// THE SINGLE-WRITER INVARIANT.
//
// Two messages racing into one conversation must produce exactly one running
// investigation. The loser is not an error: its message stays queued, and the drain at the
// running turn's terminal takes it up. This is the property that stops two agents writing
// one conversation, and it is enforced by the partial unique index rather than by any
// process, which is why it is asserted here.
func TestTwoMessagesRacingOpenExactlyOneTurn(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")

	const racers = 6
	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mutex   sync.Mutex
		turns   []conversation.Turn
		failure error
	)
	start.Add(1)
	for racer := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()

			// Both calls run OUTSIDE the mutex. The mutex guards the result slices and
			// nothing else: serialising the opens would test the invariant against one
			// caller at a time, which is the case that was never in doubt.
			_, appendErr := placements.AppendMessage(context.Background(),
				ownerOf(t, organization), organization, opened.ID,
				conversation.NewMessage{
					Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
					ActorID: "user-under-test", Text: "question " + string(rune('a'+racer)),
				})
			turn, took, openErr := placements.OpenTurn(context.Background(), organization,
				opened.ID, turnWindowLead)

			mutex.Lock()
			defer mutex.Unlock()
			if appendErr != nil {
				failure = appendErr
				return
			}
			if openErr != nil {
				failure = openErr
				return
			}
			if took {
				turns = append(turns, turn)
			}
		}()
	}
	start.Done()
	done.Wait()

	if failure != nil {
		t.Fatalf("a racer failed: %v", failure)
	}
	if len(turns) != 1 {
		t.Fatalf("%d turns opened, want exactly one; the partial unique index is the "+
			"single-writer invariant and it must refuse the rest", len(turns))
	}

	detail, err := placements.ConversationDetail(context.Background(), organization,
		opened.ID, 50)
	if err != nil {
		t.Fatalf("reading the conversation: %v", err)
	}
	if len(detail.Turns) != 1 {
		t.Errorf("the conversation holds %d turns, want one", len(detail.Turns))
	}
	if len(detail.Messages) != racers {
		t.Fatalf("the conversation holds %d messages, want %d; every racer's message is "+
			"accepted even when its turn is not", len(detail.Messages), racers)
	}
	// Whichever messages were written before the winning turn opened are attached to it;
	// the rest are queued. What must not happen is a message belonging to nothing while no
	// turn is running, because nothing would ever take it up.
	queued := 0
	for _, message := range detail.Messages {
		if message.Queued() {
			queued++
			continue
		}
		if message.InvestigationID != turns[0].InvestigationID {
			t.Errorf("message %d names turn %s, but the only turn is %s",
				message.Sequence, message.InvestigationID, turns[0].InvestigationID)
		}
	}
	if queued+countAttached(detail.Messages) != racers {
		t.Errorf("%d queued and %d attached out of %d messages", queued,
			countAttached(detail.Messages), racers)
	}
}

func countAttached(messages []conversation.Message) int {
	attached := 0
	for _, message := range messages {
		if !message.Queued() {
			attached++
		}
	}
	return attached
}

// Messages that arrived while a turn was running are drained into exactly ONE next turn,
// in order, when that turn ends. Two follow-ups typed while the agent worked are one thing
// to answer, not two investigations of the same context.
func TestQueuedMessagesDrainIntoOneNextTurn(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")

	say(t, placements, organization, opened.ID, "what changed?")
	first, took, err := placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening the first turn: took=%v err=%v", took, err)
	}

	say(t, placements, organization, opened.ID, "ignore the database")
	say(t, placements, organization, opened.ID, "check deployments instead")

	// While the first turn runs, no second turn may open.
	if _, took, err = placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead); err != nil || took {
		t.Fatalf("a second turn opened while the first was running: took=%v err=%v",
			took, err)
	}

	if err = placements.ConcludeInvestigation(context.Background(), organization,
		first.InvestigationID, conclusionSaying("nothing changed"), "",
		investigation.Spend{}); err != nil {
		t.Fatalf("concluding the first turn: %v", err)
	}

	second, took, err := placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("draining into the next turn: took=%v err=%v", took, err)
	}
	if second.Ordinal != 2 {
		t.Errorf("the drained turn is ordinal %d, want 2", second.Ordinal)
	}

	detail, err := placements.ConversationDetail(context.Background(), organization,
		opened.ID, 50)
	if err != nil {
		t.Fatalf("reading the conversation: %v", err)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("%d turns, want two: both queued messages belong to ONE next turn",
			len(detail.Turns))
	}
	for _, message := range detail.Messages {
		if message.Queued() {
			t.Errorf("message %d is still queued after the drain", message.Sequence)
		}
	}
	if detail.Messages[1].InvestigationID != second.InvestigationID ||
		detail.Messages[2].InvestigationID != second.InvestigationID {
		t.Errorf("the queued messages did not both land on the drained turn: %+v",
			detail.Messages)
	}
}

// A drain with nothing waiting is the ordinary case and not a failure. It happens at the
// end of every turn nobody interrupted.
func TestDrainingAnEmptyQueueOpensNothing(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")

	turn, took, err := placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil {
		t.Fatalf("draining an empty queue: %v", err)
	}
	if took {
		t.Errorf("a turn opened with nothing queued: %+v", turn)
	}
}

// THE TENANT BOUNDARY. An identifier from one organization supplied while acting as
// another answers not-found, with nothing that distinguishes it from an identifier that
// never existed.
func TestAnotherOrganizationsConversationIsNotFound(t *testing.T) {
	t.Parallel()

	placements, mine, theirs := twoOrganizationsOnOnePlacement(t)
	opened := openConversation(t, placements, mine, "checkout is slow")

	if _, err := placements.Conversation(context.Background(), theirs,
		opened.ID); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("reading %s as %s answered %v, want conversation unknown; a caller must "+
			"not learn that an identifier exists somewhere they cannot reach",
			opened.ID, theirs, err)
	}
	if _, err := placements.ConversationDetail(context.Background(), theirs, opened.ID,
		50); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("reading the detail across tenants answered %v", err)
	}
	if _, _, err := placements.OpenTurn(context.Background(), theirs, opened.ID,
		turnWindowLead); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("opening a turn across tenants answered %v", err)
	}
	if _, err := placements.AppendMessage(context.Background(), ownerOf(t, theirs), theirs,
		opened.ID, conversation.NewMessage{
			Role: conversation.RolePerson, ActorKind: conversation.ActorPrincipal,
			ActorID: "somebody-else", Text: "what is this about?",
		}); !errors.Is(err, conversation.ErrUnknown) {
		t.Errorf("saying something into another tenant's conversation answered %v", err)
	}
}

// The queue is countable, because the ceiling on it is what keeps overload boring.
func TestWaitingTurnsCountsUnclaimedWork(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")

	waiting, err := placements.WaitingTurns(context.Background(), organization)
	if err != nil {
		t.Fatalf("counting waiting turns: %v", err)
	}
	if waiting != 0 {
		t.Fatalf("%d waiting before anything was asked", waiting)
	}

	say(t, placements, organization, opened.ID, "what changed?")
	if _, took, openErr := placements.OpenTurn(context.Background(), organization,
		opened.ID, turnWindowLead); openErr != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, openErr)
	}

	if waiting, err = placements.WaitingTurns(
		context.Background(), organization); err != nil {
		t.Fatalf("counting waiting turns: %v", err)
	}
	if waiting != 1 {
		t.Errorf("%d waiting, want one: an unleased running turn IS the queue", waiting)
	}
}

// The listing NARROWS IN THE DATABASE. A listing that answered everything and left a
// console to filter it would be one whose paging lies: page one of a hundred conversations
// filtered down to three is not three conversations.
func TestTheConversationListingNarrowsServerSide(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	checkout := openConversation(t, placements, organization, "checkout is slow")
	openConversation(t, placements, organization, "payments are failing")
	openConversation(t, placements, organization, "checkout returns 500")

	// By subject, case-insensitively.
	listed, err := placements.QueryConversations(context.Background(),
		ownerOf(t, organization), organization, conversation.Page{Search: "CHECKOUT"})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(listed.Conversations) != 2 {
		t.Errorf("%d conversations matched %q, want 2", len(listed.Conversations),
			"CHECKOUT")
	}

	// By state.
	listed, err = placements.QueryConversations(context.Background(),
		ownerOf(t, organization), organization,
		conversation.Page{State: conversation.StateOpen})
	if err != nil {
		t.Fatalf("filtering by state: %v", err)
	}
	if len(listed.Conversations) != 3 {
		t.Errorf("%d open conversations, want 3", len(listed.Conversations))
	}
	listed, err = placements.QueryConversations(context.Background(),
		ownerOf(t, organization), organization,
		conversation.Page{State: conversation.StateClosed})
	if err != nil {
		t.Fatalf("filtering by state: %v", err)
	}
	if len(listed.Conversations) != 0 {
		t.Errorf("%d closed conversations, want none", len(listed.Conversations))
	}

	// Narrowing composes with paging, and the cursor resumes the NARROWED order rather
	// than the unnarrowed one.
	listed, err = placements.QueryConversations(context.Background(),
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
	listed, err = placements.QueryConversations(context.Background(),
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

// EPISODE-LEVEL SHARING, AND ITS BOUNDARY.
//
// Two people narrowing one incident hold separate conversations. They share the incident's
// durable fact — what its investigations ESTABLISHED, with the citations behind it — and
// nothing else. What somebody else asked, and the prose they were answered with, is theirs.
func TestConversationsOnOneEpisodeShareFindingsAndNothingElse(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	integration := kubernetesIntegration(t, placements, organization, registration)
	episode := recordEpisode(t, placements, organization, integration, "group-shared")

	// Ada's conversation about the incident, with one concluded turn.
	ada := openConversationAbout(t, placements, organization, "checkout is slow", episode)
	say(t, placements, organization, ada.ID, "ADA-PRIVATE-QUESTION: what changed?")
	adaTurn, took, err := placements.OpenTurn(context.Background(), organization, ada.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening Ada's turn: took=%v err=%v", took, err)
	}
	if err = placements.RecordToolRun(context.Background(), organization,
		adaTurn.InvestigationID, investigation.ToolRun{
			Ordinal: 1, Tool: "kubernetes.workload_runtime",
			Outcome: investigation.RunSucceeded, Summary: "1 workload",
			Sources:   []string{"checkout-api"},
			StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
		}); err != nil {
		t.Fatalf("recording Ada's run: %v", err)
	}
	if err = placements.ConcludeInvestigation(context.Background(), organization,
		adaTurn.InvestigationID, investigation.Conclusion{
			Answer: "ADA-PRIVATE-ANSWER: the pool size changed",
			Findings: []investigation.Finding{{
				Statement:  "the deploy at 14:02 changed the pool size",
				Kind:       investigation.FindingTriggeringChange,
				Confidence: investigation.ConfidenceConfirmed, Sources: []int{1},
			}},
			NextSteps: []string{"roll back the 14:02 deploy"},
		}, "", investigation.Spend{}); err != nil {
		t.Fatalf("concluding Ada's turn: %v", err)
	}

	// Bo opens a separate conversation about the SAME incident.
	bo := openConversationAbout(t, placements, organization, "why is checkout slow", episode)
	say(t, placements, organization, bo.ID, "what do we know already?")

	brief, err := placements.ConversationBrief(context.Background(), organization, bo.ID, 50)
	if err != nil {
		t.Fatalf("reading Bo's brief: %v", err)
	}

	// SHARED: the finding, with its citation intact.
	shared := false
	for _, finding := range brief.Findings {
		if finding.Statement == "the deploy at 14:02 changed the pool size" {
			shared = true
			if len(finding.Runs) == 0 {
				t.Errorf("the shared finding lost its citation: %+v", finding)
			}
			if finding.Turn != 0 {
				t.Errorf("the shared finding claims to be turn %d of THIS conversation; a "+
					"sibling turn has no ordinal here", finding.Turn)
			}
		}
	}
	if !shared {
		t.Errorf("Bo's brief carries none of the incident's established findings: %+v",
			brief.Findings)
	}

	// NOT SHARED: Ada's messages, and Ada's prose.
	for _, message := range brief.Recent {
		if strings.Contains(message.Text, "ADA-PRIVATE") {
			t.Errorf("Bo's brief carries Ada's message %q; conversations about one "+
				"incident share the incident, never each other", message.Text)
		}
	}
	for _, constraint := range brief.Summary.Constraints {
		if strings.Contains(constraint, "ADA-PRIVATE") {
			t.Errorf("Bo's brief carries Ada's instruction %q", constraint)
		}
	}

	// A conversation about a DIFFERENT incident shares nothing at all.
	other := recordEpisode(t, placements, organization, integration, "group-unrelated")
	cass := openConversationAbout(t, placements, organization, "payments are failing", other)
	unrelated, err := placements.ConversationBrief(context.Background(), organization,
		cass.ID, 50)
	if err != nil {
		t.Fatalf("reading the unrelated brief: %v", err)
	}
	if len(unrelated.Findings) != 0 {
		t.Errorf("a conversation about another incident carries %d findings from this one",
			len(unrelated.Findings))
	}
}

// openConversationAbout records one tied to an incident episode.
func openConversationAbout(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	subject string, episode uuid.UUID,
) conversation.Conversation {
	t.Helper()

	opened, err := placements.OpenConversation(context.Background(),
		ownerOf(t, organization), organization, conversation.NewConversation{
			Surface: conversation.SurfaceWeb, Subject: subject, EpisodeID: episode,
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a conversation about an episode: %v", err)
	}
	return opened
}

// What earlier turns already recommended travels too, so the tenth turn stops advising the
// rollback the second one did.
func TestTheBriefCarriesWhatEarlierTurnsAlreadyRecommended(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	opened := openConversation(t, placements, organization, "checkout is slow")
	say(t, placements, organization, opened.ID, "what changed?")
	turn, took, err := placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	if err = placements.ConcludeInvestigation(context.Background(), organization,
		turn.InvestigationID, investigation.Conclusion{
			Answer:    "the 14:02 deploy is the change",
			NextSteps: []string{"roll back the 14:02 deploy", "watch the latency panel"},
		}, "", investigation.Spend{}); err != nil {
		t.Fatalf("concluding: %v", err)
	}

	brief, err := placements.ConversationBrief(context.Background(), organization,
		opened.ID, 50)
	if err != nil {
		t.Fatalf("reading the brief: %v", err)
	}
	if len(brief.Recommended) != 2 || brief.Recommended[0] != "roll back the 14:02 deploy" {
		t.Errorf("recommended = %+v; what was already advised must travel", brief.Recommended)
	}
}

package storage_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			Source: conversation.SourceWeb, Subject: subject,
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

func startConversationTurn(t *testing.T, database *storage.Database, org, id uuid.UUID, question string) conversation.Turn {
	t.Helper()
	say(t, database, org, id, question)
	turn, started, err := openTurnForTest(database, context.Background(), org, id, turnWindowLead)
	if err != nil || !started {
		t.Fatalf("opening Conversation turn: started=%v error=%v", started, err)
	}
	return turn
}
func TestQueuedMessagesDrainIntoOneNextTurn(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "checkout is slow")

	say(t, database, organization, opened.ID, "what changed?")
	first, took, err := openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening the first turn: took=%v err=%v", took, err)
	}

	say(t, database, organization, opened.ID, "ignore the database")
	say(t, database, organization, opened.ID, "check deployments instead")

	if _, took, err = openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead); err != nil || took {
		t.Fatalf("a second turn opened while the first was running: took=%v err=%v",
			took, err)
	}

	if err = database.ConcludeInvestigation(context.Background(), organization,
		first.InvestigationID, claimToken(t, database, organization, first.InvestigationID), conclusionSaying("nothing changed"), "",
		investigation.Usage{}); err != nil {
		t.Fatalf("concluding the first turn: %v", err)
	}

	second, took, err := openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("draining into the next turn: took=%v err=%v", took, err)
	}
	if second.Ordinal != 2 {
		t.Errorf("the drained turn is ordinal %d, want 2", second.Ordinal)
	}

	detail, err := database.ConversationDetail(context.Background(), organization,
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

func TestDrainingAnEmptyQueueOpensNothing(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "checkout is slow")

	turn, took, err := openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil {
		t.Fatalf("draining an empty queue: %v", err)
	}
	if took {
		t.Errorf("a turn opened with nothing queued: %+v", turn)
	}
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

func TestConversationsOnOneIncidentShareFindingsAndNothingElse(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)
	incident := recordIncident(t, database, organization, integration, "group-shared")

	ada := openConversationAbout(t, database, organization, "checkout is slow", incident)
	say(t, database, organization, ada.ID, "ADA-PRIVATE-QUESTION: what changed?")
	adaTurn, took, err := openTurnForTest(database, context.Background(), organization, ada.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening Ada's turn: took=%v err=%v", took, err)
	}
	if err = database.RecordToolRun(context.Background(), organization,
		adaTurn.InvestigationID, claimToken(t, database, organization, adaTurn.InvestigationID), investigation.ToolRun{
			Ordinal: 1, Tool: "kubernetes_workload_runtime",
			Purpose:   "compare runtime state with the deploy",
			Arguments: map[string]any{"namespace": "shop"},
			Outcome:   investigation.RunSucceeded, Summary: "1 workload",
			Sources:   []string{"checkout-api"},
			StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
		}); err != nil {
		t.Fatalf("recording Ada's run: %v", err)
	}
	recordedRuns, err := database.InvestigationToolRuns(context.Background(),
		organization, adaTurn.InvestigationID)
	if err != nil {
		t.Fatalf("reading Ada's provenance: %v", err)
	}
	if len(recordedRuns) != 1 || recordedRuns[0].Purpose !=
		"compare runtime state with the deploy" ||
		recordedRuns[0].Arguments["namespace"] != "shop" {
		t.Fatalf("recorded purpose metadata = %+v", recordedRuns)
	}
	refresh := recordedRuns[0]
	refresh.Ordinal, refresh.Purpose = 2, ""
	refresh.Outcome, refresh.Summary = investigation.RunFailed, "source unavailable"
	refresh.Error = "source unavailable"
	if err = database.RecordToolRun(context.Background(), organization, adaTurn.InvestigationID,
		claimToken(t, database, organization, adaTurn.InvestigationID), refresh); err != nil {
		t.Fatal(err)
	}
	recordedRuns, err = database.InvestigationToolRuns(context.Background(), organization, adaTurn.InvestigationID)
	if err != nil || len(recordedRuns) != 2 || recordedRuns[1].Purpose != "" {
		t.Fatalf("an absent purpose was not preserved: runs=%+v err=%v", recordedRuns, err)
	}
	for _, ordinal := range []int{1, 2} {
		missing, err := database.EvidenceMissing(context.Background(), organization, []investigation.EvidenceRef{{
			InvestigationID: adaTurn.InvestigationID, ToolRunOrdinal: ordinal,
		}})
		if err != nil || missing != (ordinal == 2) {
			t.Fatalf("run %d evidence eligibility: missing=%v err=%v", ordinal, missing, err)
		}
	}
	if err = database.ConcludeInvestigation(context.Background(), organization,
		adaTurn.InvestigationID, claimToken(t, database, organization, adaTurn.InvestigationID), investigation.Conclusion{
			Summary: "ADA-PRIVATE-ANSWER: the pool size changed",
			Findings: []investigation.Finding{{
				Statement: "the deploy at 14:02 changed the pool size",
				Kind:      investigation.FindingObservation,
				RunRefs:   []int{1},
			}},
			Actions: []investigation.ActionProposal{{Title: "roll back the 14:02 deploy"}},
			Hypotheses: []investigation.HypothesisResult{{
				Statement: "a traffic spike may also have contributed",
				Status:    investigation.HypothesisUnresolved, Test: "compare request volume with baseline",
			}},
			Limitations: []investigation.Limitation{{
				Type:      investigation.LimitationEssentialHumanInput,
				Statement: "ADA-PRIVATE-LIMITATION: ask the payments on-call",
			}},
		}, "", investigation.Usage{}); err != nil {
		t.Fatalf("concluding Ada's turn: %v", err)
	}
	adaNext := startConversationTurn(t, database, organization, ada.ID, "explain the previous answer")
	adaBrief, err := database.ConversationBrief(context.Background(), organization, ada.ID, adaNext.InvestigationID, 50)
	if err != nil {
		t.Fatalf("reading Ada's own brief: %v", err)
	}
	if len(adaBrief.Limitations) != 1 ||
		!slices.Contains(adaBrief.OpenHypotheses, "a traffic spike may also have contributed") ||
		len(adaBrief.Recent) != 2 ||
		adaBrief.Recent[1].Answer != nil {
		t.Fatalf("Ada's conclusion prose was not persisted into her own continuity: %+v",
			adaBrief)
	}

	bo := openConversationAbout(t, database, organization, "why is checkout slow", incident)
	boTurn := startConversationTurn(t, database, organization, bo.ID, "what do we know already?")

	brief, err := database.ConversationBrief(context.Background(), organization, bo.ID, boTurn.InvestigationID, 50)
	if err != nil {
		t.Fatalf("reading Bo's brief: %v", err)
	}

	shared := false
	for _, finding := range brief.Findings {
		if finding.Statement == "the deploy at 14:02 changed the pool size" {
			shared = true
			if !strings.Contains(finding.Reference(), adaTurn.InvestigationID.String()) {
				t.Errorf("shared citation lost its originating Investigation: %s", finding.Reference())
			}
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

	for _, message := range brief.Recent {
		if strings.Contains(message.Text, "ADA-PRIVATE") || message.Answer != nil {
			t.Errorf("Bo's brief carries Ada's message %q; conversations about one "+
				"incident share the incident, never each other", message.Text)
		}
	}
	if len(brief.Limitations) != 0 {
		t.Errorf("Bo's brief carries Ada's private conclusion prose: limitations=%+v",
			brief.Limitations)
	}
	if err := database.ConcludeInvestigation(context.Background(), organization, adaNext.InvestigationID,
		claimToken(t, database, organization, adaNext.InvestigationID), investigation.Conclusion{
			Summary: "later answer",
			Findings: []investigation.Finding{{
				Statement: "a later observation must require an explicit read",
				Kind:      investigation.FindingObservation,
				RunRefs:   []int{1},
			}},
		}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := database.ConversationBrief(context.Background(), organization, bo.ID, boTurn.InvestigationID, 50)
	if err != nil || len(unchanged.Findings) != 1 || unchanged.Findings[0].InvestigationID != adaTurn.InvestigationID {
		t.Fatalf("a conclusion after Bo's Investigation opened changed initial context: %+v, error=%v", unchanged.Findings, err)
	}
	other := recordIncident(t, database, organization, integration, "group-unrelated")
	cass := openConversationAbout(t, database, organization, "payments are failing", other)
	unrelated, err := database.ConversationBrief(context.Background(), organization,
		cass.ID, startConversationTurn(t, database, organization, cass.ID, "what is known?").InvestigationID, 50)
	if err != nil {
		t.Fatalf("reading the unrelated brief: %v", err)
	}
	if len(unrelated.Findings) != 0 {
		t.Errorf("a conversation about another incident carries %d findings from this one",
			len(unrelated.Findings))
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE investigation SET concluded_at = $3
		WHERE org_id = $1 AND investigation_id = $2`, organization, adaNext.InvestigationID, boTurn.CreatedAt); err != nil {
		t.Fatal(err)
	}
	unchanged, err = database.ConversationBrief(context.Background(), organization, bo.ID, boTurn.InvestigationID, 50)
	if err != nil || len(unchanged.Findings) != 1 {
		t.Fatalf("a conclusion at the opening boundary entered prior context: %+v, error=%v", unchanged.Findings, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE investigation SET concluded_at = $3
		WHERE org_id = $1 AND investigation_id = $2`, organization, adaTurn.InvestigationID, adaNext.CreatedAt); err != nil {
		t.Fatal(err)
	}
	adaBrief, err = database.ConversationBrief(context.Background(), organization, ada.ID, adaNext.InvestigationID, 50)
	if err != nil || len(adaBrief.Findings) != 0 || len(adaBrief.OpenHypotheses) != 0 || len(adaBrief.Limitations) != 0 {
		t.Fatalf("same-Conversation context ignored the opening cutoff: %+v, error=%v", adaBrief, err)
	}
	if _, err = pool.Exec(context.Background(), `DELETE FROM investigation_tool_run WHERE org_id = $1 AND investigation_id = $2`, organization, adaTurn.InvestigationID); err != nil {
		t.Fatal(err)
	}
	retained, err := database.Investigation(context.Background(), organization, adaTurn.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	missing := false
	for _, limitation := range retained.Conclusion.Limitations {
		missing = missing || limitation.Type == investigation.LimitationMissingTelemetry
	}
	if !missing || len(retained.Conclusion.Findings[0].RunRefs) != 1 {
		t.Fatalf("pruned evidence lost its citation or limitation: %+v", retained.Conclusion)
	}
}

func openConversationAbout(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	subject string, incident uuid.UUID,
) conversation.Conversation {
	t.Helper()

	opened, err := database.OpenConversation(context.Background(),
		ownerOf(t, organization), organization, conversation.NewConversation{
			Source: conversation.SourceWeb, Subject: subject, IncidentID: incident,
			CreatedBy: "user-under-test",
		})
	if err != nil {
		t.Fatalf("opening a conversation about an incident: %v", err)
	}
	return opened
}

func TestConversationBriefKeepsOnlyTheMostRecentBoundedCitedFindings(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "bounded incident history")
	say(t, database, organization, opened.ID, "what changed?")
	turn, took, err := openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v error=%v", took, err)
	}
	findings := make([]investigation.Finding, 0, investigation.BriefMaxFindings+11)
	for index := 0; index < investigation.BriefMaxFindings+11; index++ {
		findings = append(findings, investigation.Finding{
			Statement: fmt.Sprintf("finding-%03d", index), RunRefs: []int{index + 1},
		})
	}
	if err = database.ConcludeInvestigation(context.Background(), organization,
		turn.InvestigationID, claimToken(t, database, organization, turn.InvestigationID), investigation.Conclusion{Summary: "completed", Findings: findings},
		"", investigation.Usage{}); err != nil {
		t.Fatalf("concluding the turn: %v", err)
	}
	next := startConversationTurn(t, database, organization, opened.ID, "what happened next?")
	brief, err := database.ConversationBrief(context.Background(), organization, opened.ID, next.InvestigationID, 20)
	if err != nil {
		t.Fatalf("reading the bounded brief: %v", err)
	}
	if len(brief.Findings) != investigation.BriefMaxFindings {
		t.Fatalf("prior findings = %d, want exactly %d", len(brief.Findings),
			investigation.BriefMaxFindings)
	}
	if first, last := brief.Findings[0].Statement, brief.Findings[len(brief.Findings)-1].Statement; first != "finding-011" || last != "finding-050" {
		t.Fatalf("retained prior findings span %q through %q, want the newest cited facts", first, last)
	}
	if err = database.ConcludeInvestigation(context.Background(), organization,
		next.InvestigationID, claimToken(t, database, organization, next.InvestigationID), investigation.Conclusion{
			Summary: "later finding", Findings: []investigation.Finding{
				{Statement: "newest-turn-finding", RunRefs: []int{1}},
			},
		}, "", investigation.Usage{}); err != nil {
		t.Fatalf("concluding the later turn: %v", err)
	}
	current := startConversationTurn(t, database, organization, opened.ID, "continue")
	brief, err = database.ConversationBrief(context.Background(), organization, opened.ID, current.InvestigationID, 20)
	if err != nil {
		t.Fatalf("reading the later brief: %v", err)
	}
	if len(brief.Findings) != investigation.BriefMaxFindings ||
		brief.Findings[len(brief.Findings)-1].Statement != "newest-turn-finding" {
		t.Fatalf("older findings displaced a newer turn despite the history bound: %+v", brief.Findings)
	}
}

func TestConversationHistoryRetrievesOlderFactsBeyondOneHundredMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "long-running checkout investigation")
	for sequence := 1; sequence <= 49; sequence++ {
		say(t, database, organization, opened.ID,
			fmt.Sprintf("earlier operator message %03d", sequence))
	}
	say(t, database, organization, opened.ID,
		"operator fact: production traffic stayed flat while tail latency increased")
	turn, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening first turn: took=%t error=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, organization, turn.InvestigationID,
		claimToken(t, database, organization, turn.InvestigationID), investigation.Conclusion{Summary: "telemetry remains incomplete",
			Limitations: []investigation.Limitation{
				{Type: investigation.LimitationMissingTelemetry,
					Statement: "database wait telemetry is unavailable"},
				{Type: investigation.LimitationMissingAccess,
					Statement: strings.Repeat("x", investigation.BriefMessageBound+25)},
			}}, "", investigation.Usage{}); err != nil {
		t.Fatalf("concluding first turn: %v", err)
	}
	for sequence := 1; sequence <= 100; sequence++ {
		say(t, database, organization, opened.ID,
			fmt.Sprintf("later operator message %03d", sequence))
	}
	second, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening second turn: took=%v error=%v", took, err)
	}
	if err := database.ConcludeInvestigation(ctx, organization, second.InvestigationID,
		claimToken(t, database, organization, second.InvestigationID), conclusionSaying("still investigating"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	current := startConversationTurn(t, database, organization, opened.ID, "continue")
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, current.InvestigationID,
		investigation.BriefRecentMessages)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Recent) != investigation.BriefRecentMessages {
		t.Fatalf("recent messages = %d, want bounded tail %d", len(brief.Recent),
			investigation.BriefRecentMessages)
	}
	if len(brief.Limitations) != 2 || brief.Limitations[0] != "database wait telemetry is unavailable" {
		t.Fatalf("prior limitations were lost: %+v", brief.Limitations)
	}
	if len(brief.Limitations[1]) > investigation.BriefMessageBound {
		t.Fatalf("a retained limitation is unbounded: %d characters", len(brief.Limitations[1]))
	}
	page, err := database.ConversationHistory(ctx, organization, opened.ID, current.InvestigationID, 51)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Exchange) > investigation.BriefRecentMessages+1 || page.NextBefore == 0 {
		t.Fatalf("unbounded or unpageable history: %+v", page)
	}
	foundFact := false
	for _, statement := range page.Exchange {
		foundFact = foundFact || strings.Contains(statement.Text, "production traffic stayed flat")
	}
	if !foundFact {
		t.Fatalf("selected older fact was not retrieved: %+v", page)
	}
	expected := int64(152)
	for before := brief.HistoryBefore; before != 0; {
		page, err := database.ConversationHistory(ctx, organization, opened.ID, current.InvestigationID, before)
		if err != nil || len(page.Exchange) == 0 || len(page.Exchange) > investigation.BriefRecentMessages {
			t.Fatalf("invalid history page: %+v, error=%v", page, err)
		}
		for index, entry := range page.Exchange {
			if entry.Sequence != expected-int64(len(page.Exchange)-1-index) {
				t.Fatalf("history skipped or reordered a Message: sequence=%d page=%+v", entry.Sequence, page)
			}
		}
		expected = page.Exchange[0].Sequence - 1
		if (page.NextBefore == 0) != (expected == 0) || (page.NextBefore != 0 && page.NextBefore >= before) {
			t.Fatalf("history cursor did not advance to the next older page: %+v", page)
		}
		before = page.NextBefore
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

func TestConversationContextKeepsInterleavedRepliesAndExcludesQueuedMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization, other := twoOrganizationsInOneDatabase(t)
	opened := openConversation(t, database, organization, "recent exchange")
	say(t, database, organization, opened.ID, "is production affected?")
	turn, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	say(t, database, organization, opened.ID, "correction: that was staging")
	if err = database.ConcludeInvestigation(ctx, organization, turn.InvestigationID,
		claimToken(t, database, organization, turn.InvestigationID), investigation.Conclusion{
			Summary:  "production appears affected",
			Findings: []investigation.Finding{{Statement: "production appears affected", RunRefs: []int{1}}},
			Actions: []investigation.ActionProposal{{
				Title:        "roll back",
				Rationale:    "the deployment preceded the latency increase",
				Verification: "check latency",
				RunRefs:      []int{1},
			}},
		}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	next, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening follow-up: took=%v err=%v", took, err)
	}
	say(t, database, organization, opened.ID, "future request")
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, next.InvestigationID, 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"is production affected?", "production appears affected"}
	if len(brief.Recent) != len(want) {
		t.Fatalf("recent exchange = %+v, want question, canonical answer, correction", brief.Recent)
	}
	for n, text := range want {
		if brief.Recent[n].Text != text {
			t.Fatalf("exchange[%d] = %q, want %q", n, brief.Recent[n].Text, text)
		}
	}
	if brief.Recent[0].Sequence != 1 || brief.Recent[1].Sequence != 3 ||
		brief.Recent[1].InvestigationID != turn.InvestigationID || brief.Recent[1].FromUser ||
		brief.Recent[1].CreatedAt.IsZero() || brief.Recent[1].Answer != nil {
		t.Fatalf("exchange lost source identity: %+v", brief.Recent)
	}
	limited, err := database.ConversationBrief(ctx, organization, opened.ID, next.InvestigationID, 1)
	if err != nil || len(limited.Recent) != 1 || limited.Recent[0].Text != want[1] {
		t.Fatalf("bounded exchange = %+v, err=%v", limited.Recent, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, opened.ID, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 4 || detail.Messages[2].Role != conversation.RoleAssistant {
		t.Fatalf("answer was not represented by one assistant Message: %+v", detail.Messages)
	}
	assigned, err := database.InvestigationMessages(ctx, organization, opened.ID, next.InvestigationID)
	if err != nil || len(assigned) != 1 || assigned[0].Sequence != 2 || assigned[0].Text != "correction: that was staging" {
		t.Fatalf("current request changed: %+v, error=%v", assigned, err)
	}
	history, err := database.ConversationHistory(ctx, organization, opened.ID, next.InvestigationID, 4)
	if err != nil || len(history.Exchange) != len(want) {
		t.Fatalf("older exchange = %+v, error=%v", history, err)
	}
	missing, err := database.EvidenceMissing(ctx, organization, []investigation.EvidenceRef{{
		InvestigationID: turn.InvestigationID,
		ToolRunOrdinal:  1,
	}})
	if err != nil || !missing {
		t.Fatalf("unavailable history evidence was not detected: missing=%v error=%v", missing, err)
	}
	for index, expected := range want {
		if history.Exchange[index].Text != expected {
			t.Fatalf("older exchange[%d] = %q, want %q", index, history.Exchange[index].Text, expected)
		}
	}
	answer := history.Exchange[1].Answer
	if answer == nil || len(answer.Actions) != 1 || answer.Actions[0].Rationale != "the deployment preceded the latency increase" ||
		answer.Actions[0].Verification != "check latency" || !slices.Equal(answer.Actions[0].RunRefs, []int{1}) {
		t.Fatalf("canonical action details lost: %+v", answer)
	}
	if _, err := database.ConversationBrief(ctx, other, opened.ID, next.InvestigationID, 12); err == nil {
		t.Fatal("another Organization read prior context")
	}
	if _, err := database.ConversationHistory(ctx, other, opened.ID, next.InvestigationID, 4); err == nil {
		t.Fatal("another Organization read history")
	}
	sibling := openConversation(t, database, organization, "other Conversation")
	if _, err := database.ConversationBrief(ctx, organization, sibling.ID, next.InvestigationID, 12); err == nil {
		t.Fatal("mismatched Conversation and Investigation read prior context")
	}
	if _, err := database.ConversationHistory(ctx, organization, sibling.ID, next.InvestigationID, 4); err == nil {
		t.Fatal("mismatched Conversation and Investigation read history")
	}
	if brief.HistoryBefore != 4 || limited.NextBefore != 3 {
		t.Fatalf("history range lost the interleaved reply: brief=%+v limited=%+v", brief, limited)
	}
	if err := database.ConcludeInvestigation(ctx, organization, next.InvestigationID,
		claimToken(t, database, organization, next.InvestigationID), conclusionSaying("staging confirmed"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if _, started, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead); err != nil || !started {
		t.Fatalf("opening future turn: started=%v error=%v", started, err)
	}
	repeated, err := database.ConversationHistory(ctx, organization, opened.ID, next.InvestigationID, 100)
	if err != nil || len(repeated.Exchange) != 2 || repeated.Exchange[1].Sequence != 3 || repeated.NextBefore != 0 {
		t.Fatalf("current or later turns changed previous history: %+v, error=%v", repeated, err)
	}
}

func TestRecentAnswerMarksOptionalTextTruncation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "bounded answer")
	say(t, database, organization, opened.ID, "explain")
	turn, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, organization, turn.InvestigationID,
		claimToken(t, database, organization, turn.InvestigationID), investigation.Conclusion{Summary: strings.Repeat("界", 2000)}, "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	current := startConversationTurn(t, database, organization, opened.ID, "continue")
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, current.InvestigationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Recent) != 1 || len([]rune(brief.Recent[0].Text)) > investigation.BriefMessageBound ||
		!strings.HasSuffix(brief.Recent[0].Text, " [truncated]") {
		t.Fatalf("optional answer truncation was hidden: %+v", brief.Recent)
	}
}

func TestRecentExchangePreservesMessageSequenceWhenTransactionTimesDisagree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "concurrent correction")
	say(t, database, organization, opened.ID, "production is affected")
	say(t, database, organization, opened.ID, "correction: staging is affected")
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE conversation_message
		SET created_at = '2026-09-06T10:00:00Z'::timestamptz - sequence * interval '1 second'
		WHERE org_id = $1 AND conversation_id = $2`, organization, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior, took, err := openTurnForTest(database, ctx, organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening prior turn: took=%v error=%v", took, err)
	}
	if err := database.FailInvestigation(ctx, organization, prior.InvestigationID,
		claimToken(t, database, organization, prior.InvestigationID), "telemetry unavailable", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	current := startConversationTurn(t, database, organization, opened.ID, "continue")
	brief, err := database.ConversationBrief(ctx, organization, opened.ID, current.InvestigationID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Recent) != 2 || brief.Recent[0].Sequence != 1 || brief.Recent[1].Sequence != 2 {
		t.Fatalf("transaction timestamps reversed a durable correction: %+v", brief.Recent)
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
	current, took, err := openTurnForTest(database, ctx, org, chat.Conversation, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening Slack turn: took=%v error=%v", took, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE conversation_message RENAME TO unavailable_history`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConversationBrief(ctx, org, chat.Conversation, current.InvestigationID, 10); err == nil {
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
		org, chat.Conversation); err != nil {
		t.Fatal(err)
	}
	if origin, err := database.ConversationOrigin(ctx, org, chat.Conversation); err == nil {
		t.Fatalf("missing provider binding became unrestricted origin: %+v", origin)
	}
}

func TestAtomicAppendQueuesWhileTurnIsActive(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	opened := openConversation(t, database, org, "queued follow-up")
	appendPerson := func(text string) (conversation.Message, conversation.Turn, bool, error) {
		return database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, opened.ID,
			conversation.NewMessage{Role: conversation.RoleUser,
				AuthorID: "user-under-test", AuthorName: "Test Operator", Text: text}, turnWindowLead, 100)
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
		claimToken(t, database, org, first.InvestigationID), conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	second, started, err := openTurnForTest(database, ctx, org, opened.ID, turnWindowLead)
	if err != nil || !started || second.Ordinal != 2 {
		t.Fatalf("drain: turn=%+v, started=%v, err=%v", second, started, err)
	}
	detail, err := database.ConversationDetail(ctx, org, opened.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 3 || detail.Messages[1].InvestigationID != second.InvestigationID ||
		detail.Messages[2].Role != conversation.RoleAssistant ||
		detail.Messages[2].InvestigationID != first.InvestigationID {
		t.Fatalf("persisted Messages = %+v", detail.Messages)
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
	_, _, _, err = database.AppendMessageAndOpenTurn(ctx, principal, org, recorded.Conversation,
		conversation.NewMessage{Role: conversation.RoleUser,
			AuthorID: principal.UserID().String(), AuthorName: "Web User", Text: "must not cross sources"},
		turnWindowLead, 100)
	if !errors.Is(err, conversation.ErrSourceMismatch) {
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

func TestCompetingAtomicAppendsDrainExactlyOnce(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	chat := openConversation(t, database, org, "competing follow-ups")
	principal := ownerOf(t, org)
	appendPerson := func(text string) (conversation.Turn, bool, error) {
		_, turn, started, err := database.AppendMessageAndOpenTurn(ctx, principal, org, chat.ID,
			conversation.NewMessage{Role: conversation.RoleUser,
				AuthorID: principal.UserID().String(), Text: text}, turnWindowLead, 100)
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
		claimToken(t, database, org, first.InvestigationID), conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
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
			turn, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead)
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
	if opened != 1 || len(detail.Turns) != 2 || len(detail.Messages) != 12 {
		t.Fatalf("drains=%d turns=%d Messages=%d", opened, len(detail.Turns), len(detail.Messages))
	}
	for _, message := range detail.Messages {
		if message.Role == conversation.RoleUser && message.Sequence > 1 && message.InvestigationID != second.InvestigationID {
			t.Fatalf("follow-up not assigned to the one next turn: %+v", message)
		}
	}
	if assistant := detail.Messages[len(detail.Messages)-1]; assistant.Role != conversation.RoleAssistant ||
		assistant.InvestigationID != first.InvestigationID {
		t.Fatalf("first reply was not retained as one assistant Message: %+v", assistant)
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
	said.ContentDigest = randomDigest(t)
	said.Text = "retry after capacity is available"
	if _, err := database.RecordSlackMessage(ctx, org, said); !errors.Is(err, conversation.ErrQueueFull) {
		t.Fatalf("new Message at capacity: %v", err)
	}
	if _, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead); err != nil || !started {
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
			_, _, _, err = database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, chat.ID,
				conversation.NewMessage{Role: conversation.RoleUser,
					AuthorID: "user-under-test", Text: "must roll back"}, turnWindowLead, 100)
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
		if _, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead); err != nil || !started {
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
				conversation.NewMessage{Role: conversation.RoleUser,
					AuthorID: "user-under-test", Text: "last available slot"}, turnWindowLead, 100)
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
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message
		WHERE org_id = $1 AND investigation_id IS NULL AND role = 1`, org).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 100 {
		t.Fatalf("persisted queued Messages = %d, want 100", queued)
	}
}

func TestQueuedMessageBacklogDrainsInBoundedOrderedBatches(t *testing.T) {
	database, org, _ := twoOrganizationsInOneDatabase(t)
	ctx := context.Background()
	chat := openConversation(t, database, org, "queued backlog")
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_message
		(conversation_id, org_id, sequence, role, author_id, author_name, text, window_from, window_until)
		SELECT $1, $2, n, 1, 'actor-' || n, 'Operator', 'question-' || n, now() - interval '24 hours', now()
		FROM generate_series(1, 205) n`, chat.ID, org); err != nil {
		t.Fatal(err)
	}
	for batch, want := range []int{100, 100, 5} {
		turn, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead)
		if err != nil || !started {
			t.Fatalf("batch %d: started=%v, err=%v", batch, started, err)
		}
		var count, first, last int
		if err := pool.QueryRow(ctx, `SELECT count(*), min(sequence), max(sequence)
			FROM conversation_message WHERE org_id = $1 AND conversation_id = $2 AND investigation_id = $3`,
			org, chat.ID, turn.InvestigationID).Scan(&count, &first, &last); err != nil {
			t.Fatal(err)
		}
		if count != want || first != batch*100+1 || last != batch*100+want {
			t.Fatalf("batch %d: count=%d first=%d last=%d", batch, count, first, last)
		}
		if err := database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
			claimToken(t, database, org, turn.InvestigationID), conclusionSaying("answer"), "", investigation.Usage{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, started, err := openTurnForTest(database, ctx, org, chat.ID, turnWindowLead); err != nil || started {
		t.Fatalf("empty drain: started=%v, err=%v", started, err)
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
		if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
			claimToken(t, database, org, turn.InvestigationID), conclusionSaying("answer"), "", investigation.Usage{}); err != nil {
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

func TestQueuedMessageWindowSurvivesDrain(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			chat := openConversation(t, database, org, "queued windows")
			appendMessage := func(window *conversation.Window) (conversation.Message, conversation.Turn, bool, error) {
				return database.AppendMessageAndOpenTurn(ctx, ownerOf(t, org), org, chat.ID,
					conversation.NewMessage{Role: conversation.RoleUser, AuthorID: "user-under-test", Text: "question", Window: window}, time.Hour, 100)
			}
			_, first, opened, err := appendMessage(nil)
			if err != nil || !opened {
				t.Fatalf("first: %v %v", opened, err)
			}
			var requested *conversation.Window
			if explicit {
				requested = &conversation.Window{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
			}
			queued, _, opened, err := appendMessage(requested)
			if err != nil || opened || queued.WindowFrom.IsZero() || queued.WindowUntil.IsZero() {
				t.Fatalf("queued window: %+v opened=%v err=%v", queued, opened, err)
			}
			inherited, _, _, err := appendMessage(nil)
			if err != nil || !inherited.WindowFrom.Equal(queued.WindowFrom) || !inherited.WindowUntil.Equal(queued.WindowUntil) {
				t.Fatalf("window inheritance: %+v err=%v", inherited, err)
			}
			_, _, _, err = appendMessage(&conversation.Window{From: queued.WindowFrom.Add(-time.Hour), Until: queued.WindowUntil})
			if !errors.Is(err, conversation.ErrWindowConflict) {
				t.Fatalf("incompatible window accepted: %v", err)
			}
			if _, err = database.CancelInvestigation(ctx, ownerOf(t, org), org, first.InvestigationID); err != nil {
				t.Fatal(err)
			}
			second, opened, err := openTurnForTest(database, ctx, org, chat.ID, 365*24*time.Hour)
			if err != nil || !opened {
				t.Fatalf("drain: %v %v", opened, err)
			}
			found, err := database.Investigation(ctx, org, second.InvestigationID)
			if err != nil || !found.WindowFrom.Equal(queued.WindowFrom) || !found.WindowUntil.Equal(queued.WindowUntil) {
				t.Fatalf("drain shifted window: %+v err=%v", found, err)
			}
			detail, err := database.ConversationDetail(ctx, org, chat.ID, 10)
			if err != nil || len(detail.Messages) != 3 {
				t.Fatalf("refusal wrote a Message: %d %v", len(detail.Messages), err)
			}
		})
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

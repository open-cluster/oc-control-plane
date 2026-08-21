package investigation

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE CONTEXT CEILING, AND THE BRIEF THAT FEEDS IT.

// A turn whose own transcript outgrows the budget is STOPPED honestly: the conclusion is
// forced and the record says `context`. It does not fail, and it is not silently truncated
// — "we stopped" is never dressed up as "we found nothing".
func TestATurnPastItsContextBudgetIsStoppedNotFailed(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	// One read that returns far more than the budget allows.
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			Content: []string{strings.Repeat("x", 8_000)}, Summary: "a great deal",
		}, nil
	})

	read := AgentCall{ID: "call-1", Tool: "stub.read",
		Arguments: map[string]any{"channel": "deploys"}}
	// One read, then the conclusion the forced turn must produce. The budget is spent by
	// the first read, so the second turn arrives with the tools already withdrawn.
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{read}},
		{Conclusion: &Conclusion{Answer: "as much as could be read"}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: &scriptedInvestigator{exchange: exchange},
		// The CEILING, which is what ends a turn. The budget below it would only have
		// compacted, and a turn with nothing to compact — this one has no conversation —
		// would have carried on reading.
		ContextCeiling: 1_000,
		Logger:         slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if store.status != StatusConcluded {
		t.Fatalf("status = %v; a turn that filled its context still concludes, with what "+
			"it established", store.status)
	}
	if store.stoppedBy != StoppedByContext {
		t.Errorf("stoppedBy = %q, want %q; running out of context room must be said in "+
			"the same honest way as running out of budget", store.stoppedBy,
			StoppedByContext)
	}
	if store.answer == "" {
		t.Errorf("a stopped turn produced no answer at all")
	}
	// Exactly two turns: the read, and the forced conclusion. A third would mean the
	// ceiling did not fire when the budget was spent.
	if len(exchange.fed) != 2 {
		t.Errorf("%d turns; the ceiling must fire on the turn after the budget is spent",
			len(exchange.fed))
	}
	if !exchange.fed[1].mustConclude {
		t.Errorf("the second turn was not forced to conclude: %+v", exchange.fed[1])
	}
	if !strings.Contains(exchange.fed[1].reason, "working context") {
		t.Errorf("the model was told %q; it must be told why its reads are over",
			exchange.fed[1].reason)
	}
}

// A turn inside its budget is untouched. The ceiling is a backstop, not a leash.
func TestATurnInsideItsContextBudgetConcludesFreely(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"small"}, Summary: "1 deploy"}, nil
	})

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator:  &scriptedInvestigator{exchange: oneRead()},
		ContextBudget: 1_000_000,
		Logger:        slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if store.status != StatusConcluded || store.stoppedBy != "" {
		t.Errorf("status=%v stoppedBy=%q; a turn well inside its budget concludes freely",
			store.status, store.stoppedBy)
	}
}

// A turn belonging to a conversation is oriented with its brief, so a follow-up knows what
// the turns before it established and what it was told to do.
func TestAConversationTurnIsOrientedWithItsBrief(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		candidates: []integrations.Integration{stubIntegration("Deploy Slack")},
		brief:      aBrief(),
	}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	investigator := &scriptedInvestigator{exchange: oneRead()}
	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: investigator,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "checkout latency",
		ConversationID: uuid.New(), Turn: 4,
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	brief := investigator.orientation.Brief
	if brief == nil {
		t.Fatal("the turn was oriented with no brief; a follow-up that knows nothing is a " +
			"first question asked twice")
	}
	if brief.Turn != 4 {
		t.Errorf("brief.Turn = %d, want 4", brief.Turn)
	}
	if len(brief.Findings) != 4 {
		t.Errorf("the brief carried %d prior findings, want 4", len(brief.Findings))
	}
}

// A single-shot investigation has no conversation, so it is oriented with no brief and
// nothing is read for one.
func TestASingleShotInvestigationIsOrientedWithoutABrief(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		candidates: []integrations.Integration{stubIntegration("Deploy Slack")},
		briefFails: true,
	}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	investigator := &scriptedInvestigator{exchange: oneRead()}
	runAutonomousWith(t, store, catalog, investigator)

	if investigator.orientation.Brief != nil {
		t.Errorf("a single-shot investigation was given a brief: %+v",
			investigator.orientation.Brief)
	}
	if store.status != StatusConcluded {
		t.Errorf("status = %v", store.status)
	}
}

// A brief that cannot be read NARROWS the turn rather than failing it, exactly as an
// unreadable trigger or ledger already does. A follow-up with no memory is worse than one
// with it, and better than none at all.
func TestAnUnreadableBriefNarrowsTheTurnRatherThanFailingIt(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		candidates: []integrations.Integration{stubIntegration("Deploy Slack")},
		briefFails: true,
	}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	investigator := &scriptedInvestigator{exchange: oneRead()}
	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: investigator,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "checkout latency",
		ConversationID: uuid.New(), Turn: 2,
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if store.status != StatusConcluded {
		t.Errorf("status = %v; an unreadable brief must not fail the turn", store.status)
	}
	if investigator.orientation.Brief != nil {
		t.Errorf("a brief was carried despite the read failing")
	}
}

// runAutonomousWith is runAutonomous with the investigator handed back, for the tests that
// assert on the orientation it received.
func runAutonomousWith(
	t *testing.T, store *memoryStore, catalog integrations.Catalog,
	investigator Investigator,
) {
	t.Helper()
	runAutonomous(t, store, catalog, investigator)
}

// THE GAP BETWEEN THE TWO NUMBERS is what a compaction buys.
//
// The budget and the ceiling were one number, and the consequence was structural: the
// ceiling compares the whole carried context — the conversation AND the tool catalogue,
// which is never zero — so it was always crossed first. Every turn that compacted was a
// turn already told to conclude, and compaction could only ever help the NEXT turn. It
// never bought the turn holding the transcript any room to carry on reading.
//
// This is that turn: carrying more than the compaction threshold and less than the
// ceiling. It must read again, not conclude.
func TestATurnOverTheCompactionThresholdKeepsReading(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			Content: []string{strings.Repeat("x", 6_000)}, Summary: "a good deal",
		}, nil
	})

	first := AgentCall{ID: "call-1", Tool: "stub.read",
		Arguments: map[string]any{"channel": "deploys"}}
	second := AgentCall{ID: "call-2", Tool: "stub.read",
		Arguments: map[string]any{"channel": "incidents"}}
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{first}},
		{Calls: []AgentCall{second}},
		{Conclusion: &Conclusion{Answer: "read twice, then concluded"}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: exchange},
		// The first read lands above the compaction threshold and below the ceiling.
		ContextBudget:  1_000,
		ContextCeiling: 100_000,
		Logger:         slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if store.status != StatusConcluded {
		t.Fatalf("status = %v", store.status)
	}
	if store.stoppedBy != "" {
		t.Errorf("stoppedBy = %q, want none: a turn between the two numbers has room to "+
			"work, and stopping it there is the defect", store.stoppedBy)
	}
	// Three turns: two reads and the conclusion. Two would mean the ceiling fired on the
	// turn that should have carried on reading.
	if len(exchange.fed) != 3 {
		t.Fatalf("%d turns, want 3; the second read is the room the gap exists to buy",
			len(exchange.fed))
	}
	if exchange.fed[1].mustConclude {
		t.Error("the second turn was forced to conclude while still under the ceiling")
	}
}

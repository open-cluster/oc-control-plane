package investigation

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
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
		{Conclusion: &Conclusion{Summary: "as much as could be read"}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: &scriptedInvestigator{exchange: exchange},
		// The ceiling ends this turn without requiring a Conversation or rewriting history.
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

func TestAProviderConversationCarriesItsThreadIntoEveryToolRequest(t *testing.T) {
	t.Parallel()

	origin := stubIntegration("Origin workspace")
	store := &memoryStore{
		candidates: []integrations.Integration{origin},
		brief: Brief{
			OriginIntegrationID: origin.ID.String(),
			OriginChannel:       "C-INCIDENT", OriginThread: "1710000000.1",
		},
	}
	var seen integrations.ToolRequest
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{{
				Name: "stub.read", Description: "reads its originating thread",
				WhenToUse: "for this thread", WhenNotToUse: "for another thread",
				Permissions: "history", Output: "messages", ConversationScoped: true,
				Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
					seen = request
					return integrations.ToolResult{Summary: "one thread message"}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: oneRead()},
		Logger:       slog.New(slog.DiscardHandler),
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "checkout latency", ConversationID: uuid.New(), Turn: 1,
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if seen.OriginChannel != "C-INCIDENT" || seen.OriginThread != "1710000000.1" {
		t.Fatalf("the actual Tool request lost its originating thread: %+v", seen)
	}
}

func TestConversationOrientationNeverPersistsOrReplacesCitedFindingsWithASummary(t *testing.T) {
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
		ContextBudget: 1,
		Logger:        slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "checkout latency", ConversationID: uuid.New(), Turn: 4,
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if investigator.orientation.Brief == nil || len(investigator.orientation.Brief.Findings) != 4 {
		t.Fatalf("the follow-up must retain prior cited findings: %+v", investigator.orientation.Brief)
	}
}

func aBrief() Brief {
	return Brief{
		ConversationID: "c-1", Subject: "checkout latency", Turn: 4,
		Recent: []BriefMessage{
			{FromPerson: true, Actor: "Ada", Text: "ignore the database, look at deployments"},
			{FromPerson: false, Actor: "OpenCluster", Text: "the deploy at 14:02 is the change"},
			{FromPerson: true, Actor: "Ada", Text: "what contradicts the cache hypothesis?"},
		},
		RecentFrom: 7,
		Findings: []PriorFinding{
			{Turn: 1, Statement: "the deploy at 14:02 changed the pool size",
				Kind: FindingTrigger, Confidence: ConfidenceConfirmed, Runs: []int{2, 3}},
			{Turn: 2, Statement: "the database was not saturated",
				Kind: FindingRuledOut, Confidence: ConfidenceConfirmed, Runs: []int{1}},
			{Turn: 3, Statement: "whether the cache warmed is unknown",
				Kind: FindingUnresolved, Confidence: ConfidencePossible, Runs: []int{4}},
			{Turn: 3, Statement: "the deployed revision is v2.14.1",
				Kind: FindingObservation, Confidence: ConfidenceConfirmed, Runs: []int{5}},
		},
		FailedReads: []string{"the metrics endpoint returned 503"},
		Identifiers: []string{"C0DEPLOYS", "octo/checkout-api"},
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

// A turn above the soft budget but below the hard ceiling can continue gathering evidence.
func TestATurnAboveItsSoftBudgetAndBelowItsCeilingKeepsReading(t *testing.T) {
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
		{Conclusion: &Conclusion{Summary: "read twice, then concluded"}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: exchange},
		// The first read exceeds the soft budget while remaining below the hard ceiling.
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

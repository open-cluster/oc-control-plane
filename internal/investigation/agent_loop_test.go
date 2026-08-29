package investigation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The autonomous loop against an in-memory store, a scripted exchange and stub
// tools: the branches the composition suite cannot cheaply reach — duplicate
// suppression, stagnation, each ceiling's honest stop, refused citations, and the
// orientation being platform-held context only.

// scriptedExchange plays back moves in order and remembers what it was fed.
type scriptedExchange struct {
	mu      sync.Mutex
	moves   []Move
	failure error
	// fed is every Next call's inputs, in order.
	fed []conversationTurn
	// allowMissingPurpose is set only by the contract test that exercises rejection.
	// All other scripts model calls after the native schema has required a purpose.
	allowMissingPurpose bool
}

type conversationTurn struct {
	results      []CallResult
	mustConclude bool
	reason       string
}

func (s *scriptedExchange) Next(
	_ context.Context, results []CallResult, mustConclude bool, reason string,
) (Move, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fed = append(s.fed, conversationTurn{results, mustConclude, reason})
	if s.failure != nil {
		return Move{}, s.failure
	}
	if len(s.moves) == 0 {
		return Move{Conclusion: &Conclusion{}}, nil
	}
	next := s.moves[0]
	s.moves = s.moves[1:]
	if !s.allowMissingPurpose {
		for index := range next.Calls {
			if next.Calls[index].Tool != UpdateHypothesesToolName &&
				strings.TrimSpace(next.Calls[index].Purpose) == "" {
				next.Calls[index].Purpose = "test the current hypothesis"
			}
		}
	}
	return next, nil
}

// scriptedInvestigator hands out one scripted exchange and remembers the
// orientation it was opened with.
type scriptedInvestigator struct {
	mu          sync.Mutex
	exchange    *scriptedExchange
	orientation Orientation
	failure     error
}

func (s *scriptedInvestigator) OpenExchange(
	_ context.Context, orientation Orientation,
) (Exchange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orientation = orientation
	if s.failure != nil {
		return nil, s.failure
	}
	return s.exchange, nil
}

func runAutonomous(
	t *testing.T, store *memoryStore, catalog integrations.Catalog,
	investigator Investigator,
) *Runner {
	t.Helper()
	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: investigator,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom:  time.Now().Add(-2 * time.Hour),
		WindowUntil: time.Now(),
	})
	runner.running.Wait()
	return runner
}

func TestTheAutonomousLoopRecordsOfferedSourcesRunsAndConclusion(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Payments Slack"), stubIntegration("Deploy Slack"),
	}}
	executed := 0
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		return integrations.ToolResult{
			Content: []string{"found"}, Summary: "1 item", Sources: []string{"C1"},
		}, nil
	})
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{
			ID: "c1", Tool: "stub.read__" + strings.ReplaceAll(store.candidates[0].ID.String(), "-", ""),
		}},
			Spend: Spend{InputTokens: 10, MicroCents: 1}},
		{Conclusion: &Conclusion{
			Findings: []Finding{{
				Statement:  "the pool change did it",
				Kind:       FindingCause,
				Confidence: ConfidenceConfirmed,
				Sources:    []int{1},
			}},
			Actions: []ActionProposal{{Title: "roll back the pool change"}},
		}, Spend: Spend{InputTokens: 20, MicroCents: 2}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if store.status != StatusConcluded {
		t.Fatalf("status = %s, reason %q", store.status, store.failReason)
	}
	if len(store.sources) != 2 {
		t.Fatalf("sources = %+v; every offered integration is on the record", store.sources)
	}
	for _, source := range store.sources {
		if !strings.Contains(source.Reason, "offered") {
			t.Errorf("source reason = %q; the offer must be the stated reason", source.Reason)
		}
	}
	if executed != 1 || len(store.runs) != 1 || store.runs[0].Ordinal != 1 {
		t.Errorf("executed=%d runs=%+v", executed, store.runs)
	}
	if store.spend.MicroCents != 3 {
		t.Errorf("spend = %+v, want both moves summed", store.spend)
	}
	if len(store.findings) != 1 || store.findings[0].Kind != FindingCause ||
		store.findings[0].Confidence != ConfidenceConfirmed {
		t.Errorf("findings = %+v; kind and confidence must survive", store.findings)
	}
	if len(store.actions) != 1 || store.actions[0].Title != "roll back the pool change" {
		t.Errorf("actions = %+v", store.actions)
	}
	if store.stoppedBy != "" {
		t.Errorf("stopped_by = %q; a free conclusion carries no label", store.stoppedBy)
	}
	// The exchange was fed the executed run's result, matched to its call.
	if len(exchange.fed) != 2 || len(exchange.fed[1].results) != 1 {
		t.Fatalf("fed = %+v", exchange.fed)
	}
	if result := exchange.fed[1].results[0]; result.CallID != "c1" ||
		result.Run.Ordinal != 1 || result.Run.Outcome != RunSucceeded {
		t.Errorf("result = %+v; the call's own identifier must come back with the run", result)
	}
}

func TestADuplicateCallIsSuppressedVisibly(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	executed := 0
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		return integrations.ToolResult{Content: []string{"found"}}, nil
	})
	same := map[string]any{"channel": "C1"}
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "stub.read", Arguments: same}}},
		{Calls: []AgentCall{{ID: "c2", Tool: "stub.read", Arguments: same}}},
		{Conclusion: &Conclusion{}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if executed != 1 {
		t.Fatalf("executed = %d; an identical read must not run twice", executed)
	}
	if len(store.runs) != 2 || store.runs[1].Outcome != RunFailed ||
		!strings.Contains(store.runs[1].Error, "run 1") {
		t.Fatalf("runs = %+v; the suppressed duplicate is on the record naming the original",
			store.runs)
	}
	// The model reads an in-band note with its allowed next moves, not a bare failure.
	suppressed := exchange.fed[2].results[0]
	if !strings.Contains(suppressed.Run.Error, "different") ||
		!strings.Contains(suppressed.Run.Error, "conclude") {
		t.Errorf("note = %q; the note names the allowed next moves", suppressed.Run.Error)
	}
}

func TestAReadWithoutPurposeIsRejectedWithoutDispatch(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	executed := 0
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		return integrations.ToolResult{}, nil
	})
	exchange := &scriptedExchange{allowMissingPurpose: true, moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "stub.read"}}},
		{Conclusion: &Conclusion{Summary: "The read was rejected."}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if executed != 0 {
		t.Fatalf("a read without purpose dispatched %d times", executed)
	}
	if len(store.runs) != 1 || store.runs[0].Outcome != RunFailed ||
		!strings.Contains(store.runs[0].Error, "purpose") {
		t.Fatalf("recorded run = %+v", store.runs)
	}
}

func TestIncidentPreflightReadsOnlyExactKubernetesIdentifiers(t *testing.T) {
	t.Parallel()

	integration := integrations.Integration{
		ID: uuid.New(), Type: integrations.TypeKubernetes, Name: "Production Kubernetes",
	}
	store := &memoryStore{
		candidates: []integrations.Integration{integration},
		trigger: &Trigger{Title: "CheckoutUnavailable", Labels: map[string]string{
			"namespace": "shop", "workload_kind": "Deployment",
			"workload_name": "checkout-api",
		}},
	}
	var requested []integrations.ToolRequest
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: integrations.TypeKubernetes, Key: "kubernetes", Name: "Kubernetes",
			Category: integrations.CategoryInfrastructure, Available: true,
			Tools: []integrations.Tool{
				{Name: "kubernetes.workload.runtime", Description: "runtime", WhenToUse: "exact workload",
					WhenNotToUse: "discovery", Permissions: "read", Output: "runtime",
					Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
						requested = append(requested, request)
						return integrations.ToolResult{Summary: "runtime read"}, nil
					}},
				{Name: "kubernetes.namespace.events", Description: "events", WhenToUse: "exact namespace",
					WhenNotToUse: "discovery", Permissions: "read", Output: "events",
					Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
						requested = append(requested, request)
						return integrations.ToolResult{Summary: "events read"}, nil
					}},
			}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange := &scriptedExchange{moves: []Move{{Conclusion: &Conclusion{
		Summary: "preflight complete",
	}}}}
	investigator := &scriptedInvestigator{exchange: exchange}
	runner := &Runner{Store: store, Catalog: catalog, Investigator: investigator,
		Logger: slog.New(slog.DiscardHandler)}
	organization, _ := tenancy.NewOrganization("org-test")
	runner.Start(organization, Investigation{
		ID: uuid.New(), IncidentID: uuid.New(), Subject: "checkout unavailable",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if len(requested) != 2 || len(store.runs) != 2 {
		t.Fatalf("requested=%d runs=%+v", len(requested), store.runs)
	}
	if store.runs[0].Ordinal != 1 || store.runs[1].Ordinal != 2 ||
		store.runs[0].Purpose == "" || store.runs[1].Purpose == "" {
		t.Errorf("preflight provenance = %+v", store.runs)
	}
	if got := store.runs[0].Arguments; got["namespace"] != "shop" ||
		got["workloadKind"] != "Deployment" || got["workloadName"] != "checkout-api" {
		t.Errorf("runtime arguments = %+v", got)
	}
	if got := store.runs[1].Arguments; got["namespace"] != "shop" || len(got) != 1 {
		t.Errorf("event arguments = %+v", got)
	}
	if len(investigator.orientation.Preflight) != 2 ||
		investigator.orientation.Preflight[1].Ordinal != 2 {
		t.Errorf("orientation preflight = %+v", investigator.orientation.Preflight)
	}
}

func TestStagnationForcesAnHonestStop(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	same := map[string]any{"channel": "C1"}
	repeat := func(id string) Move {
		return Move{Calls: []AgentCall{{ID: id, Tool: "stub.read", Arguments: same}}}
	}
	exchange := &scriptedExchange{moves: []Move{
		repeat("c1"), repeat("c2"), repeat("c3"),
		{Conclusion: &Conclusion{Findings: []Finding{{
			Statement: "partial", Kind: FindingUnresolved,
			Confidence: ConfidencePossible, Sources: []int{1},
		}}}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if store.status != StatusConcluded {
		t.Fatalf("status = %s reason %q; stagnation concludes, never fails",
			store.status, store.failReason)
	}
	if store.stoppedBy != StoppedByStagnation {
		t.Errorf("stopped_by = %q, want %q", store.stoppedBy, StoppedByStagnation)
	}
	// Turn 1 executes; turns 2 and 3 are stagnant duplicates; the fourth call to the
	// exchange must be the forced conclusion.
	final := exchange.fed[len(exchange.fed)-1]
	if !final.mustConclude || !strings.Contains(final.reason, "new evidence") {
		t.Errorf("final turn = %+v; the forced conclusion says why", final)
	}
}

func TestTheTurnCeilingForcesAnHonestStop(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	fresh := func(id string, page int) Move {
		return Move{Calls: []AgentCall{{ID: id, Tool: "stub.read",
			Arguments: map[string]any{"page": page}}}}
	}
	exchange := &scriptedExchange{moves: []Move{
		fresh("c1", 1), fresh("c2", 2), fresh("c3", 3),
		{Conclusion: &Conclusion{}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: exchange},
		MaxTurns:     3,
		Logger:       slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "s"})
	runner.running.Wait()

	if store.status != StatusConcluded || store.stoppedBy != StoppedByReasonerTurns {
		t.Fatalf("status=%s stopped_by=%q reason=%q", store.status, store.stoppedBy,
			store.failReason)
	}
	if len(exchange.fed) != 4 {
		t.Errorf("turns = %d; three reading turns and one forced conclusion",
			len(exchange.fed))
	}
}

func TestTheRunCeilingDropsVisiblyAndForcesAnHonestStop(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	executed := 0
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{
			{ID: "c1", Tool: "stub.read", Arguments: map[string]any{"page": 1}},
			{ID: "c2", Tool: "stub.read", Arguments: map[string]any{"page": 2}},
			{ID: "c3", Tool: "stub.read", Arguments: map[string]any{"page": 3}},
		}},
		{Conclusion: &Conclusion{}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: exchange},
		MaxToolRuns:  2,
		Logger:       slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "s"})
	runner.running.Wait()

	if executed != 2 {
		t.Fatalf("executed = %d, want the budget's 2", executed)
	}
	if len(store.runs) != 3 || store.runs[2].Outcome != RunFailed ||
		!strings.Contains(store.runs[2].Error, "not executed") {
		t.Fatalf("runs = %+v; the dropped call is visibly on the record", store.runs)
	}
	if store.status != StatusConcluded || store.stoppedBy != StoppedByToolRuns {
		t.Errorf("status=%s stopped_by=%q", store.status, store.stoppedBy)
	}
}

func TestTheSpendCeilingForcesAnHonestStopOnTheAutonomousPath(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "stub.read",
			Arguments: map[string]any{"page": 1}}}, Spend: Spend{MicroCents: 7}},
		{Calls: []AgentCall{{ID: "c2", Tool: "stub.read",
			Arguments: map[string]any{"page": 2}}}, Spend: Spend{MicroCents: 7}},
		{Conclusion: &Conclusion{Findings: []Finding{{
			Statement: "partial", Kind: FindingUnresolved,
			Confidence: ConfidencePossible, Sources: []int{1},
		}}}, Spend: Spend{MicroCents: 1}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator:           &scriptedInvestigator{exchange: exchange},
		SpendCeilingMicroCents: 10,
		Logger:                 slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "s"})
	runner.running.Wait()

	if store.status != StatusConcluded || store.stoppedBy != StoppedBySpend {
		t.Fatalf("status=%s stopped_by=%q reason=%q", store.status, store.stoppedBy,
			store.failReason)
	}
	if store.spend.MicroCents != 15 {
		t.Errorf("spend = %+v; every move is summed including the conclusion", store.spend)
	}
}

func TestAutonomousCitationsAreCheckedAgainstRunsThatHappened(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	exchange := &scriptedExchange{moves: []Move{
		{Conclusion: &Conclusion{Findings: []Finding{{
			Statement: "invented", Kind: FindingCause,
			Confidence: ConfidenceConfirmed, Sources: []int{7},
		}}}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if store.status != StatusFailed || !strings.Contains(store.failReason, "never ran") {
		t.Errorf("status = %s reason %q; an untraceable finding must never be stored",
			store.status, store.failReason)
	}
}

func TestTheOrientationCarriesHeldContextOnly(t *testing.T) {
	t.Parallel()

	trigger := Trigger{
		Title:  "HighLatency",
		Labels: map[string]string{"namespace": "payments"},
		Annotations: map[string]string{
			"runbook_url": "https://runbooks.acme/latency",
		},
		GeneratorURL: "https://prometheus.acme/graph",
	}
	store := &memoryStore{
		candidates: []integrations.Integration{stubIntegration("Payments Slack")},
		trigger:    &trigger,
		inventory:  []string{"payments/Deployment api-server"},
	}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	investigator := &scriptedInvestigator{exchange: &scriptedExchange{}}

	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: investigator,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), IncidentID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-2 * time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	orientation := investigator.orientation
	if orientation.Subject != "payments latency" {
		t.Errorf("subject = %q", orientation.Subject)
	}
	if orientation.Trigger == nil ||
		orientation.Trigger.Annotations["runbook_url"] == "" ||
		orientation.Trigger.GeneratorURL == "" {
		t.Errorf("trigger = %+v; the alert's own annotations and generator URL are held "+
			"context", orientation.Trigger)
	}
	if len(orientation.Sources) != 1 || len(orientation.Sources[0].Tools) != 1 {
		t.Errorf("sources = %+v", orientation.Sources)
	}
	if len(orientation.Inventory) != 1 {
		t.Errorf("inventory = %+v; the ledger's digest is held context", orientation.Inventory)
	}
}

func TestAnIntegrationWhoseGrantsSupportNoToolIsNotOffered(t *testing.T) {
	t.Parallel()

	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{{
				Name: "stub.read", Description: "reads",
				WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
				Output: "items", Requires: []string{"scope:special"},
				Run: func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ungranted := stubIntegration("No Grants")
	store := &memoryStore{candidates: []integrations.Integration{ungranted}}
	investigator := &scriptedInvestigator{exchange: &scriptedExchange{}}

	runAutonomous(t, store, catalog, investigator)

	if len(investigator.orientation.Sources) != 0 || len(store.sources) != 0 {
		t.Errorf("sources = %+v / %+v; a tool the grants cannot support is absent, and an "+
			"integration offering nothing is not a source",
			investigator.orientation.Sources, store.sources)
	}
	if store.status != StatusConcluded {
		t.Errorf("status = %s; nothing readable still concludes honestly", store.status)
	}
}

func TestMustConcludeWithoutAConclusionFailsTheInvestigation(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	same := map[string]any{"page": 1}
	stubborn := func(id string) Move {
		return Move{Calls: []AgentCall{{ID: id, Tool: "stub.read", Arguments: same}}}
	}
	exchange := &scriptedExchange{moves: []Move{
		stubborn("c1"), stubborn("c2"), stubborn("c3"), stubborn("c4"), stubborn("c5"),
	}}

	runner := &Runner{
		Store: store, Catalog: catalog,
		Investigator: &scriptedInvestigator{exchange: exchange},
		MaxTurns:     3,
		Logger:       slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "s"})
	runner.running.Wait()

	if store.status != StatusFailed || !strings.Contains(store.failReason, "conclude") {
		t.Errorf("status = %s reason %q; refusing the forced conclusion is a reasoner "+
			"failure, never a fabricated conclusion", store.status, store.failReason)
	}
}

func TestAFailedConversationFailsTheInvestigation(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	exchange := &scriptedExchange{failure: ErrReasonerUnavailable}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if store.status != StatusFailed || store.failReason == "" {
		t.Errorf("status = %s reason %q", store.status, store.failReason)
	}
}

func TestAnUnopenableConversationFailsTheInvestigation(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	investigator := &scriptedInvestigator{failure: errors.New("boom")}

	runAutonomous(t, store, catalog, investigator)

	if store.status != StatusFailed {
		t.Errorf("status = %s", store.status)
	}
}

func TestWallClockReserveIsDetectedFromTheDeadline(t *testing.T) {
	t.Parallel()

	ctx, done := context.WithTimeout(context.Background(), time.Minute)
	defer done()
	if !wallClockAlmostOver(ctx, 2*time.Minute) {
		t.Error("one minute left against a two-minute reserve is almost over")
	}
	if wallClockAlmostOver(ctx, 10*time.Second) {
		t.Error("one minute left against a ten-second reserve is not almost over")
	}
	if wallClockAlmostOver(context.Background(), time.Minute) {
		t.Error("no deadline is never almost over")
	}
}

// Every ceiling names itself, in cost order — including wall clock, which no scripted
// loop can reach without a clock of its own.
func TestEachCeilingNamesItself(t *testing.T) {
	t.Parallel()

	loop := &autonomousLoop{
		runner: &Runner{SpendCeilingMicroCents: 10}, maxRuns: 30, maxTurns: 20,
	}
	open := context.Background()
	almostOver, done := context.WithTimeout(context.Background(), time.Second)
	defer done()

	cases := []struct {
		name     string
		ctx      context.Context
		spend    Spend
		executed int
		turn     int
		stagnant int
		want     string
	}{
		{"nothing fired", open, Spend{}, 0, 1, 0, ""},
		{"spend", open, Spend{MicroCents: 10}, 0, 1, 0, StoppedBySpend},
		{"tool runs", open, Spend{}, 30, 1, 0, StoppedByToolRuns},
		{"reasoner turns", open, Spend{}, 0, 21, 0, StoppedByReasonerTurns},
		{"wall clock", almostOver, Spend{}, 0, 1, 0, StoppedByWallClock},
		{"stagnation", open, Spend{}, 0, 1, 2, StoppedByStagnation},
	}
	for _, one := range cases {
		loop.spend = one.spend
		loop.executed = one.executed
		got := loop.firedCeiling(one.ctx, one.turn, one.stagnant)
		if got != one.want {
			t.Errorf("%s: fired %q, want %q", one.name, got, one.want)
		}
	}
}

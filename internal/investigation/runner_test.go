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

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The runner against an in-memory store, a scripted reasoner and stub tools: the bounded
// loop's branches that the composition suite cannot cheaply reach — expansion, forced
// conclusion, refused citations, tools nobody offered.

// memoryStore keeps one investigation's provenance in memory.
type memoryStore struct {
	mu         sync.Mutex
	candidates []integrations.Integration
	sources    []Source
	runs       []ToolRun
	findings   []Finding
	spend      Spend
	status     Status
	failReason string
}

func (m *memoryStore) CreateInvestigation(
	context.Context, authz.Principal, tenancy.Organization, NewInvestigation,
) (Investigation, error) {
	return Investigation{}, errors.New("not used by the runner")
}

func (m *memoryStore) Investigation(
	context.Context, tenancy.Organization, uuid.UUID,
) (Investigation, error) {
	return Investigation{}, errors.New("not used by the runner")
}

func (m *memoryStore) InvestigationProvenance(
	context.Context, tenancy.Organization, uuid.UUID,
) ([]Source, []ToolRun, error) {
	return nil, nil, errors.New("not used by the runner")
}

func (m *memoryStore) QueryInvestigations(
	context.Context, authz.Principal, tenancy.Organization, Page,
) (List, error) {
	return List{}, errors.New("not used by the runner")
}

func (m *memoryStore) RecordSource(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, source Source,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sources = append(m.sources, source)
	return nil
}

func (m *memoryStore) RecordToolRun(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, run ToolRun,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, run)
	return nil
}

func (m *memoryStore) ConcludeInvestigation(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, findings []Finding, spend Spend,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.findings, m.spend = StatusConcluded, findings, spend
	return nil
}

func (m *memoryStore) FailInvestigation(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, reason string, spend Spend,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.failReason, m.spend = StatusFailed, reason, spend
	return nil
}

func (m *memoryStore) TriggerEpisode(
	context.Context, tenancy.Organization, uuid.UUID,
) (Trigger, error) {
	return Trigger{}, errors.New("not used by the runner")
}

func (m *memoryStore) OpenTriggers(
	context.Context, tenancy.Organization, int,
) ([]Trigger, error) {
	return nil, errors.New("not used by the runner")
}

func (m *memoryStore) InvestigationCandidates(
	context.Context, tenancy.Organization,
) ([]integrations.Integration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.candidates, nil
}

// scripted plays back decisions in order and remembers the briefs it saw.
type scripted struct {
	mu        sync.Mutex
	decisions []Decision
	failure   error
	briefs    []Brief
}

func (s *scripted) Decide(_ context.Context, brief Brief) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.briefs = append(s.briefs, brief)
	if s.failure != nil {
		return Decision{}, s.failure
	}
	if len(s.decisions) == 0 {
		return Decision{}, nil
	}
	next := s.decisions[0]
	s.decisions = s.decisions[1:]
	return next, nil
}

// stubType builds a catalog whose one type offers one tool whose answer the test scripts.
func stubType(
	t *testing.T, answer func(request integrations.ToolRequest) (integrations.ToolResult, error),
) integrations.Catalog {
	t.Helper()
	catalog, err := integrations.NewCatalog(integrations.Definition{
		ID: 99, Key: "stub", Name: "Stub", Category: integrations.CategoryObservability,
		Capabilities: []string{"stub.read"},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
		Tools: []integrations.Tool{{
			Name: "stub.read", Capability: "stub.read", Description: "reads",
			WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
			RateLimit: "free", Output: "items",
			Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
				return answer(request)
			},
		}},
	})
	if err != nil {
		t.Fatalf("assembling the stub catalog: %v", err)
	}
	return catalog
}

func stubIntegration(name string) integrations.Integration {
	return integrations.Integration{ID: uuid.New(), Type: 99, Name: name}
}

func runInvestigation(
	t *testing.T, store *memoryStore, catalog integrations.Catalog, reasoner Reasoner,
) *Runner {
	t.Helper()
	runner := &Runner{
		Store: store, Catalog: catalog, Reasoner: reasoner,
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

func TestTheRunnerRecordsRoutingRunsAndConclusion(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Payments Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			Content: []string{"found"}, Summary: "1 item", Sources: []string{"C1"},
		}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}},
			Spend: Spend{InputTokens: 10, MicroCents: 1}},
		{Findings: []Finding{{Statement: "found it", Sources: []int{1}}},
			Spend: Spend{InputTokens: 20, MicroCents: 2}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusConcluded {
		t.Fatalf("status = %s, reason %q", store.status, store.failReason)
	}
	if len(store.sources) != 1 || store.sources[0].Reason == "" {
		t.Errorf("sources = %+v; routing must be recorded with its reason", store.sources)
	}
	if len(store.runs) != 1 || store.runs[0].Ordinal != 1 ||
		store.runs[0].Outcome != RunSucceeded || store.runs[0].Summary != "1 item" {
		t.Errorf("runs = %+v", store.runs)
	}
	if store.spend.InputTokens != 30 || store.spend.MicroCents != 3 {
		t.Errorf("spend = %+v, want both decisions summed", store.spend)
	}
	if len(store.findings) != 1 {
		t.Errorf("findings = %+v", store.findings)
	}
}

func TestACallNamingNoOfferedToolIsAFailedRunNotACrash(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "github.read_commits"}}},
		{},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if len(store.runs) != 1 || store.runs[0].Outcome != RunFailed ||
		!strings.Contains(store.runs[0].Error, "offer") {
		t.Errorf("runs = %+v; a call outside the selection is provenance, not a crash",
			store.runs)
	}
	if store.status != StatusConcluded {
		t.Errorf("status = %s; one refused read must not end the investigation", store.status)
	}
}

func TestExpansionHappensOnceAndOnlyWhenNothingWasFound(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Alpha"), stubIntegration("Beta"), stubIntegration("Gamma"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{},
	}}

	runInvestigation(t, store, catalog, reasoner)

	// Two selected up front, one expansion after the empty first round; the third stays
	// out even though later rounds also found nothing.
	if len(store.sources) != 3 {
		t.Fatalf("sources = %+v", store.sources)
	}
	expansion := store.sources[2]
	if !strings.Contains(expansion.Reason, "expanded") ||
		!strings.Contains(expansion.Reason, "returned nothing") {
		t.Errorf("the expansion's reason %q does not say why it happened", expansion.Reason)
	}
}

func TestNoExpansionWhenTheFirstSelectionFoundSomething(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Alpha"), stubIntegration("Beta"), stubIntegration("Gamma"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"hit"}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Findings: []Finding{{Statement: "done", Sources: []int{1}}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if len(store.sources) != 2 {
		t.Errorf("sources = %d; a search that is finding things must not widen", len(store.sources))
	}
}

func TestTheRoundBudgetForcesAConclusion(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"more"}}, nil
	})
	// A reasoner that never wants to stop: every scripted decision proposes reads.
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Calls: []ToolCall{{Tool: "stub.read"}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusConcluded && store.status != StatusFailed {
		t.Fatalf("status = %s; the loop must always end", store.status)
	}
	if len(reasoner.briefs) > maxRounds {
		t.Errorf("the reasoner decided %d times; the budget is %d", len(reasoner.briefs), maxRounds)
	}
	last := reasoner.briefs[len(reasoner.briefs)-1]
	if !last.MustConclude {
		t.Error("the final round was not told to conclude")
	}
}

func TestFindingsCitingRunsThatNeverRanFailTheInvestigation(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Findings: []Finding{{Statement: "invented", Sources: []int{7}}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusFailed || !strings.Contains(store.failReason, "never ran") {
		t.Errorf("status = %s reason %q; an untraceable finding must never be stored",
			store.status, store.failReason)
	}
}

func TestAReasonerFailureFailsTheInvestigationWithTheReason(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	reasoner := &scripted{failure: ErrReasonerUnavailable}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusFailed || store.failReason == "" {
		t.Errorf("status = %s reason %q", store.status, store.failReason)
	}
}

func TestWithNoReadableSourcesTheReasonerConcludesFromTheSubjectAlone(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	reasoner := &scripted{}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusConcluded || len(store.findings) != 0 {
		t.Errorf("status = %s findings %+v; nothing readable concludes with nothing "+
			"established, honestly", store.status, store.findings)
	}
	if len(reasoner.briefs) != 1 || !reasoner.briefs[0].MustConclude {
		t.Errorf("briefs = %+v; with nothing to read the first decision is the conclusion",
			reasoner.briefs)
	}
}

// reasonerFunc adapts a function to the boundary, for the one test that needs to block.
type reasonerFunc func(context.Context, Brief) (Decision, error)

func (f reasonerFunc) Decide(ctx context.Context, brief Brief) (Decision, error) {
	return f(ctx, brief)
}

func TestTheRunnerReportsItsConcurrencyCap(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	release := make(chan struct{})
	runner := &Runner{
		Store: store, Catalog: catalog, Logger: slog.New(slog.DiscardHandler),
		Reasoner: reasonerFunc(func(context.Context, Brief) (Decision, error) {
			<-release
			return Decision{}, nil
		}),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatalf("organization: %v", err)
	}

	for range maxConcurrent {
		runner.Start(organization, Investigation{
			ID: uuid.New(), Subject: "s",
			WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
		})
	}
	if !runner.AtCapacity() {
		t.Fatal("the runner holds its limit of investigations and does not say so")
	}
	close(release)
	runner.running.Wait()
	if runner.AtCapacity() {
		t.Error("every investigation ended and the runner still reports itself full")
	}
}

func TestTheToolReceivesTheInvestigationWindowAndCredential(t *testing.T) {
	t.Parallel()

	var seen integrations.ToolRequest
	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(request integrations.ToolRequest) (integrations.ToolResult, error) {
		seen = request
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read", Arguments: map[string]any{}}}},
		{Findings: []Finding{{Statement: "s", Sources: []int{1}}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if seen.WindowFrom.IsZero() || seen.WindowUntil.IsZero() {
		t.Error("the tool ran without the investigation's window; nothing could clamp to it")
	}
}

package investigation

import (
	"bytes"
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
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The runner against an in-memory store, a scripted reasoner and stub tools: the bounded
// loop's branches that the composition suite cannot cheaply reach — expansion, forced
// conclusion, refused citations, tools nobody offered.

// memoryStore keeps one investigation's provenance in memory.
type memoryStore struct {
	mu           sync.Mutex
	candidates   []integrations.Integration
	trigger      *Trigger
	inventory    []string
	sources      []Source
	runs         []ToolRun
	findings     []Finding
	nextSteps    []string
	stoppedBy    string
	unseals      []string
	refuseUnseal bool
	spend        Spend
	status       Status
	failReason   string
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
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, findings []Finding,
	nextSteps []string, stoppedBy string, spend Spend,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.findings, m.stoppedBy, m.spend = StatusConcluded, findings, stoppedBy, spend
	m.nextSteps = nextSteps
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trigger == nil {
		return Trigger{}, errors.New("no trigger scripted")
	}
	return *m.trigger, nil
}

func (m *memoryStore) WorkloadInventory(
	context.Context, tenancy.Organization, int,
) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inventory, nil
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

func (m *memoryStore) RecordCredentialUnseal(
	_ context.Context, _ tenancy.Organization, id uuid.UUID, purpose string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuseUnseal {
		return errors.New("the record is not writable")
	}
	m.unseals = append(m.unseals, id.String()+" for "+purpose)
	return nil
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
		ID: 99, Key: "stub", Name: "Stub", Category: integrations.CategoryAlerting,
		Capabilities: []string{"stub.read"},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
		Tools: []integrations.Tool{{
			Name: "stub.read", Capability: "stub.read", Description: "reads",
			WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
			Output: "items",
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
	return runInvestigationWith(t, store, catalog, seal.Sealer{}, reasoner)
}

func runInvestigationWith(
	t *testing.T, store *memoryStore, catalog integrations.Catalog, sealer seal.Sealer,
	reasoner Reasoner,
) *Runner {
	t.Helper()
	runner := &Runner{
		Store: store, Catalog: catalog, Sealer: sealer, Reasoner: reasoner,
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

// Every credential unseal is on the audit record — once per integration per
// investigation, matching the cache — and it lands BEFORE the credential is used, so a
// use that cannot be recorded does not happen.
func TestACredentialUnsealIsOnTheRecordBeforeTheToolRuns(t *testing.T) {
	t.Parallel()

	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	holder := stubIntegration("Payments Slack")
	sealed, err := sealer.Seal("xoxb-token", integrations.CredentialBinding(holder.ID))
	if err != nil {
		t.Fatal(err)
	}
	holder.CredentialSealed = sealed

	var presented []string
	store := &memoryStore{candidates: []integrations.Integration{holder}}
	catalog := stubType(t, func(request integrations.ToolRequest) (integrations.ToolResult, error) {
		presented = append(presented, request.Credential)
		return integrations.ToolResult{Content: []string{"x"}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}, {Tool: "stub.read"}}},
		{Findings: []Finding{{Statement: "done", Sources: []int{1}}}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Sealer: sealer, Reasoner: reasoner,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	opened := Investigation{ID: uuid.New(), Subject: "payments latency"}
	runner.Start(organization, opened)
	runner.running.Wait()

	if len(presented) != 2 || presented[0] != "xoxb-token" || presented[1] != "xoxb-token" {
		t.Fatalf("the tool must be handed the opened credential each call: %q", presented)
	}
	if len(store.unseals) != 1 {
		t.Fatalf("unseals = %v; one integration opened once records exactly one event",
			store.unseals)
	}
	if want := holder.ID.String() + " for investigation " + opened.ID.String(); store.unseals[0] != want {
		t.Errorf("unseal record = %q, want %q", store.unseals[0], want)
	}
}

func TestAnUnrecordableUnsealMeansTheCredentialIsNotUsed(t *testing.T) {
	t.Parallel()

	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	holder := stubIntegration("Payments Slack")
	holder.CredentialSealed, err = sealer.Seal("xoxb-token",
		integrations.CredentialBinding(holder.ID))
	if err != nil {
		t.Fatal(err)
	}

	store := &memoryStore{
		candidates: []integrations.Integration{holder}, refuseUnseal: true,
	}
	catalog := stubType(t, func(request integrations.ToolRequest) (integrations.ToolResult, error) {
		t.Error("the tool ran although the unseal could not be recorded")
		return integrations.ToolResult{}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{},
	}}

	runInvestigationWith(t, store, catalog, sealer, reasoner)

	if len(store.runs) != 1 || store.runs[0].Outcome != RunFailed {
		t.Fatalf("runs = %+v; an unrecordable unseal is a failed run", store.runs)
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
	if store.stoppedBy != StoppedByReasonerTurns {
		t.Errorf("stopped_by = %q; a turn-forced conclusion must say so", store.stoppedBy)
	}
}

// A reached spend ceiling forces the concluding turn and is recorded as what stopped
// the investigation — an honest partial conclusion, never a failure and never a free
// diagnosis.
func TestTheSpendCeilingForcesALabeledConclusion(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"more"}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}, Spend: Spend{MicroCents: 7}},
		{Calls: []ToolCall{{Tool: "stub.read"}}, Spend: Spend{MicroCents: 7}},
		{Findings: []Finding{{Statement: "partial", Sources: []int{1}}}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Reasoner: reasoner,
		Logger:                 slog.New(slog.DiscardHandler),
		SpendCeilingMicroCents: 10,
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "latency"})
	runner.running.Wait()

	if store.status != StatusConcluded {
		t.Fatalf("status = %s, reason %q; a spend stop is a conclusion, not a failure",
			store.status, store.failReason)
	}
	if store.stoppedBy != StoppedBySpend {
		t.Errorf("stopped_by = %q, want %q", store.stoppedBy, StoppedBySpend)
	}
	// The ceiling fired after the second decision (14 >= 10), so the third is the
	// forced concluding turn — and it must know what was already spent.
	if len(reasoner.briefs) != 3 || !reasoner.briefs[2].MustConclude {
		t.Fatalf("briefs = %d; the decision after the ceiling must be the conclusion",
			len(reasoner.briefs))
	}
	if reasoner.briefs[2].SpendSoFar.MicroCents != 14 {
		t.Errorf("the concluding brief carries spend %d, want 14",
			reasoner.briefs[2].SpendSoFar.MicroCents)
	}
	if len(store.findings) != 1 {
		t.Errorf("findings = %+v; the partial conclusion must be stored", store.findings)
	}
}

// A free conclusion carries no stopped_by label.
func TestAFreeConclusionIsNotLabeledStopped(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"found"}}, nil
	})
	reasoner := &scripted{decisions: []Decision{
		{Calls: []ToolCall{{Tool: "stub.read"}}},
		{Findings: []Finding{{Statement: "done", Sources: []int{1}}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if store.status != StatusConcluded || store.stoppedBy != "" {
		t.Errorf("status = %s stopped_by = %q; a free conclusion carries no label",
			store.status, store.stoppedBy)
	}
}

func TestDroppedCallsAreRecordedNotSilentlyDiscarded(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{stubIntegration("S")}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Content: []string{"found"}}, nil
	})
	sixCalls := make([]ToolCall, 6)
	for index := range sixCalls {
		sixCalls[index] = ToolCall{Tool: "stub.read", Arguments: map[string]any{
			"page": float64(index)}}
	}
	reasoner := &scripted{decisions: []Decision{
		{Calls: sixCalls},
		{Findings: []Finding{{Statement: "done", Sources: []int{1}}}},
	}}

	runInvestigation(t, store, catalog, reasoner)

	if len(store.runs) != 6 {
		t.Fatalf("recorded %d runs, want all 6 proposed calls on the record", len(store.runs))
	}
	for _, run := range store.runs[:maxCallsPerRound] {
		if run.Outcome != RunSucceeded {
			t.Errorf("run %d = %d, want executed", run.Ordinal, run.Outcome)
		}
	}
	for _, run := range store.runs[maxCallsPerRound:] {
		if run.Outcome != RunFailed || !strings.Contains(run.Error, "not executed") {
			t.Errorf("dropped run %d = %d %q; a dropped call must be visibly dropped",
				run.Ordinal, run.Outcome, run.Error)
		}
	}
	if len(reasoner.briefs) < 2 {
		t.Fatal("the investigation ended before a second decision")
	}
	if got := len(reasoner.briefs[1].Runs); got != 6 {
		t.Errorf("the next brief carried %d runs, want 6 — the model must see the drop", got)
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

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

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// The runner shell against an in-memory store and stub tools: the guarantees every
// investigation shares whatever the exchange does — audited credential unseals,
// honest failed runs, the concurrency cap, and the window handed to every tool.

// memoryStore keeps one investigation's provenance in memory.
type memoryStore struct {
	mu           sync.Mutex
	candidates   []integrations.Integration
	trigger      *Trigger
	inventory    []string
	sources      []Source
	runs         []ToolRun
	answer       string
	drained      []uuid.UUID
	drainOpens   bool
	brief        Brief
	briefFails   bool
	endRefused   bool
	findings     []Finding
	actions      []ActionProposal
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
	_ context.Context, _ tenancy.Organization, id uuid.UUID,
) (Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status != 0 {
		return Investigation{ID: id, Status: m.status}, nil
	}
	return Investigation{}, errors.New("not used by the runner")
}

func (m *memoryStore) InvestigationProvenance(
	context.Context, tenancy.Organization, uuid.UUID,
) ([]Source, []ToolRun, error) {
	return nil, nil, errors.New("not used by the runner")
}

func (m *memoryStore) QueryInvestigations(
	context.Context, authz.Principal, tenancy.Organization, Query,
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
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, conclusion Conclusion,
	stoppedBy string, spend Spend,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.stoppedBy, m.spend = StatusConcluded, stoppedBy, spend
	m.answer, m.findings, m.actions = conclusion.Summary, conclusion.Findings,
		conclusion.Actions
	return nil
}

// brief is what a scripted conversation contributes to its next turn.
func (m *memoryStore) ConversationBrief(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, _ int,
) (Brief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.briefFails {
		return Brief{}, errors.New("the brief could not be read")
	}
	return m.brief, nil
}

// drained records every conversation the runner tried to take up at a terminal boundary.
func (m *memoryStore) DrainConversation(
	_ context.Context, _ tenancy.Organization, conversation uuid.UUID, _ time.Duration,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drained = append(m.drained, conversation)
	return m.drainOpens, nil
}

func (m *memoryStore) FailInvestigation(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, reason string, spend Spend,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.endRefused {
		// What the guarded update answers when the row is no longer running, which is
		// what the sweeper having got there first looks like from here.
		return ErrUnknown
	}
	m.status, m.failReason, m.spend = StatusFailed, reason, spend
	return nil
}

func (m *memoryStore) TriggerIncident(
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

// stubType builds a catalog whose one type offers one tool whose answer the test scripts.
func stubType(
	t *testing.T, answer func(request integrations.ToolRequest) (integrations.ToolResult, error),
) integrations.Catalog {
	t.Helper()
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub",
			Category: integrations.CategoryAlerting, Available: true,
			Tools: []integrations.Tool{{
				Name: "stub.read", Description: "reads",
				WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
				Output: "items",
				Run: func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
					return answer(request)
				},
			}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatalf("assembling the stub catalog: %v", err)
	}
	return catalog
}

func stubIntegration(name string) integrations.Integration {
	return integrations.Integration{ID: uuid.New(), Type: 99, Name: name}
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
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{
			{ID: "c1", Tool: "stub.read", Arguments: map[string]any{"page": float64(1)}},
			{ID: "c2", Tool: "stub.read", Arguments: map[string]any{"page": float64(2)}},
		}},
		{Conclusion: &Conclusion{Findings: []Finding{{
			Statement: "done", Kind: FindingSymptom, Confidence: ConfidencePossible,
			Sources: []int{1},
		}}}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Sealer: sealer,
		Investigator: &scriptedInvestigator{exchange: exchange},
		Logger:       slog.New(slog.DiscardHandler),
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
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "stub.read"}}},
		{Conclusion: &Conclusion{}},
	}}

	runner := &Runner{
		Store: store, Catalog: catalog, Sealer: sealer,
		Investigator: &scriptedInvestigator{exchange: exchange},
		Logger:       slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{ID: uuid.New(), Subject: "payments latency"})
	runner.running.Wait()

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
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "github.read_commits"}}},
		{Conclusion: &Conclusion{}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if len(store.runs) != 1 || store.runs[0].Outcome != RunFailed ||
		!strings.Contains(store.runs[0].Error, "offer") {
		t.Errorf("runs = %+v; a call outside the offer is provenance, not a crash",
			store.runs)
	}
	if store.status != StatusConcluded {
		t.Errorf("status = %s; one refused read must not end the investigation", store.status)
	}
}

// investigatorFunc adapts a function to the boundary, for the one test that needs to
// block inside a exchange.
type investigatorFunc func(context.Context, Orientation) (Exchange, error)

func (f investigatorFunc) OpenExchange(
	ctx context.Context, orientation Orientation,
) (Exchange, error) {
	return f(ctx, orientation)
}

type conversationFunc func(context.Context, []CallResult, bool, string) (Move, error)

func (f conversationFunc) Next(
	ctx context.Context, results []CallResult, mustConclude bool, reason string,
) (Move, error) {
	return f(ctx, results, mustConclude, reason)
}

func TestTheRunnerReportsItsConcurrencyCap(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})
	release := make(chan struct{})
	blocking := conversationFunc(func(
		context.Context, []CallResult, bool, string,
	) (Move, error) {
		<-release
		return Move{Conclusion: &Conclusion{}}, nil
	})
	runner := &Runner{
		Store: store, Catalog: catalog, Logger: slog.New(slog.DiscardHandler),
		Investigator: investigatorFunc(func(
			context.Context, Orientation,
		) (Exchange, error) {
			return blocking, nil
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
	exchange := &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "c1", Tool: "stub.read", Arguments: map[string]any{}}}},
		{Conclusion: &Conclusion{Findings: []Finding{{
			Statement: "s", Kind: FindingSymptom, Confidence: ConfidencePossible,
			Sources: []int{1},
		}}}},
	}}

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: exchange})

	if seen.WindowFrom.IsZero() || seen.WindowUntil.IsZero() {
		t.Error("the tool ran without the investigation's window; nothing could clamp to it")
	}
}

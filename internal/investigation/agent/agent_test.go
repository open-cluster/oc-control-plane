package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedModel struct {
	mu    sync.Mutex
	calls int
	next  func(int, Prompt) (Completion, error)
	size  func(Prompt) (int, error)
}

func (m *scriptedModel) Complete(_ context.Context, prompt Prompt) (Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.next(m.calls, prompt)
}

func (m *scriptedModel) RequestTokens(prompt Prompt) (int, error) {
	if m.size != nil {
		return m.size(prompt)
	}
	return EstimatePromptTokens(prompt)
}

type records struct {
	mu          sync.Mutex
	candidate   integrations.Integration
	trigger     investigation.Trigger
	brief       investigation.Brief
	history     investigation.HistoryPage
	briefErr    error
	origin      *investigation.ConversationOrigin
	originErr   error
	messages    []investigation.AssignedMessage
	messagesErr error
	runs        []investigation.ToolRun
	unseals     int
	toolUsed    bool
	conclusion  investigation.Conclusion
	status      investigation.Status
	terminalErr error
	auditErr    error
	eventErr    error
	events      []investigation.Event
	failure     string
	stoppedBy   string
	usage       investigation.Usage
}

func (r *records) RecordToolRun(_ context.Context, _ tenancy.Organization, _ uuid.UUID, _ uuid.UUID, run investigation.ToolRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, run)
	return nil
}
func (r *records) ConcludeInvestigation(_ context.Context, _ tenancy.Organization, _ uuid.UUID, _ uuid.UUID, conclusion investigation.Conclusion, stoppedBy string, usage investigation.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalErr != nil {
		return r.terminalErr
	}
	r.conclusion, r.status, r.stoppedBy, r.usage = conclusion, investigation.StatusConcluded, stoppedBy, usage
	return r.appendTerminalPayload(investigation.ConcludedPayload(conclusion, stoppedBy))
}
func (r *records) FailInvestigation(_ context.Context, _ tenancy.Organization, _ uuid.UUID, _ uuid.UUID, reason string, usage investigation.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalErr != nil {
		return r.terminalErr
	}
	r.status, r.failure, r.usage = investigation.StatusFailed, reason, usage
	return r.appendTerminalPayload(investigation.FailedPayload(reason))
}

func (r *records) appendTerminalPayload(payload investigation.EventPayload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	r.events = append(r.events, investigation.Event{Sequence: int64(len(r.events) + 1), At: time.Now(),
		Type: payload.EventType(), Payload: fields})
	return nil
}
func (r *records) TriggerIncident(context.Context, tenancy.Organization, uuid.UUID) (investigation.Trigger, error) {
	if r.trigger.IncidentID == uuid.Nil {
		return investigation.Trigger{}, errors.New("unused")
	}
	return r.trigger, nil
}
func (r *records) InvestigationCandidates(context.Context, tenancy.Organization) ([]integrations.Integration, error) {
	return []integrations.Integration{r.candidate}, nil
}
func (r *records) RecordCredentialUnseal(context.Context, tenancy.Organization, uuid.UUID, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unseals++
	return r.auditErr
}
func (r *records) WorkloadInventory(context.Context, tenancy.Organization, int) ([]string, error) {
	return nil, nil
}
func (r *records) ConversationBrief(context.Context, tenancy.Organization, uuid.UUID, int) (investigation.Brief, error) {
	return r.brief, r.briefErr
}

func (r *records) ConversationHistory(context.Context, tenancy.Organization, uuid.UUID, int64) (investigation.HistoryPage, error) {
	return r.history, nil
}

func (r *records) ConversationOrigin(context.Context, tenancy.Organization, uuid.UUID) (*investigation.ConversationOrigin, error) {
	return r.origin, r.originErr
}

func (r *records) InvestigationMessages(context.Context, tenancy.Organization, uuid.UUID, uuid.UUID) ([]investigation.AssignedMessage, error) {
	return r.messages, r.messagesErr
}
func (r *records) AppendEvent(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, _ uuid.UUID, event investigation.Event,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventErr == nil {
		r.events = append(r.events, event)
	}
	return r.eventErr
}
func testCatalog(t *testing.T, run func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error)) integrations.Catalog {
	t.Helper()
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: 99, Key: "stub", Name: "Stub", Category: integrations.CategoryAlerting, Available: true, Tools: []integrations.Tool{{Name: "stub.read", Description: "Read a value.", WhenToUse: "To answer.", WhenNotToUse: "Never.", Permissions: "read", Output: "value", Run: run}}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func validConclusion(t *testing.T, refs []int) json.RawMessage {
	t.Helper()
	findings := []map[string]any{}
	if len(refs) > 0 {
		findings = append(findings, map[string]any{"id": "f1", "statement": "The deployed value is v2.", "kind": "observation", "confidence": "confirmed", "mechanism": "", "run_refs": refs})
	}
	document := map[string]any{
		"status": "answer_only", "summary": "The deployed value is v2.",
		"impact":     map[string]any{"status": "unknown", "current_state": "unknown", "affected_services": []string{}, "affected_users": []string{}, "summary": "impact is unknown.", "run_refs": []int{}},
		"findings":   findings,
		"hypotheses": []any{}, "actions": []any{}, "limitations": []any{},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func configuredTestAgent(t *testing.T, store *records, model Model, catalog integrations.Catalog) *Agent {
	t.Helper()
	return &Agent{model: model, deployment: Deployment{Provider: "scripted", Model: "test", ContextWindowTokens: 128_000, MaxOutputTokens: 1024}, Store: store, Catalog: catalog, Logger: slog.New(slog.DiscardHandler)}
}

func TestRunAcceptsACustomModelWithExplicitLimits(t *testing.T) {
	store := &records{}
	model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	built, err := NewAgent(Deployment{
		Provider: "anthropic", Model: "claude-future-release",
		ContextWindowTokens: 128_000, MaxOutputTokens: 1_024,
	}, model)
	if err != nil {
		t.Fatal(err)
	}
	built.Store = store
	built.Catalog = testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		})
	built.Logger = slog.New(slog.DiscardHandler)

	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err = built.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || store.status != investigation.StatusConcluded {
		t.Fatalf("model calls=%d status=%s", model.calls, store.status)
	}
}

func TestRunRecordsToolEvidenceBeforeTheNextModelCall(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	catalog := testCatalog(t, func(_ context.Context, _ integrations.ToolRequest) (integrations.ToolResult, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.toolUsed = true
		return integrations.ToolResult{Summary: "v2", Content: map[string]any{"version": "v2"}}, nil
	})
	model := &scriptedModel{}
	model.next = func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "c1", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)}}}, nil
		}
		store.mu.Lock()
		recorded := len(store.runs) == 1 && store.toolUsed
		store.mu.Unlock()
		if !recorded {
			t.Fatal("the Tool Run was not durable before the next Model call")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1})}}}, nil
	}
	agent := configuredTestAgent(t, store, model, catalog)
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "deployment", WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || len(store.runs) != 1 {
		t.Fatalf("status=%s runs=%d", store.status, len(store.runs))
	}
}

func TestModelChoosesReadsBeforeWorkloadLabelsCauseExternalAccess(t *testing.T) {
	integrationID, incidentID := uuid.New(), uuid.New()
	store := &records{
		candidate: integrations.Integration{ID: integrationID, Type: integrations.TypeKubernetes, Name: "cluster"},
		trigger: investigation.Trigger{IncidentID: incidentID, Labels: map[string]string{
			"namespace": "payments", "workload_kind": "Deployment", "workload_name": "checkout-api",
		}},
	}
	modelSelected := false
	run := func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
		if !modelSelected {
			t.Fatal("workload labels triggered an external read before model selection")
		}
		return integrations.ToolResult{Summary: request.Arguments["namespace"].(string)}, nil
	}
	tools := []integrations.Tool{
		{Name: "kubernetes.workload.runtime", Description: "runtime", WhenToUse: "runtime state", WhenNotToUse: "never", Permissions: "read", Output: "state", Run: run},
		{Name: "kubernetes.namespace.events", Description: "events", WhenToUse: "recent events", WhenNotToUse: "never", Permissions: "read", Output: "events", Run: run},
		{Name: "kubernetes.pod.logs", Description: "logs", WhenToUse: "diagnosis", WhenNotToUse: "never", Permissions: "read", Output: "logs", Run: run},
	}
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Manifest: integrations.Manifest{ID: integrations.TypeKubernetes, Key: "kubernetes", Name: "Kubernetes", Category: integrations.CategoryInfrastructure, Available: true, Tools: tools},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusActive}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			for _, expected := range []string{"kubernetes.workload.runtime", "kubernetes.namespace.events", "kubernetes.pod.logs"} {
				found := false
				for _, offered := range prompt.Tools {
					found = found || offered.Name == expected
				}
				if !found {
					t.Fatalf("eligible tool %q missing from model choice", expected)
				}
			}
			modelSelected = true
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "logs", Name: "kubernetes.pod.logs", Arguments: json.RawMessage(`{"purpose":"read logs","input":{"namespace":"payments"}}`),
			}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1}),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{
		ID: uuid.New(), IncidentID: incidentID, Subject: "checkout-api",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.runs) != 1 || store.runs[0].Tool != "kubernetes.pod.logs" {
		t.Fatalf("runs=%+v, want only the model-selected read", store.runs)
	}
	for index, recorded := range store.runs {
		if recorded.Ordinal != index+1 {
			t.Fatalf("run %d ordinal=%d", index, recorded.Ordinal)
		}
	}
}

func TestRunScopesAProviderConversationToItsOriginThread(t *testing.T) {
	for _, historyAvailable := range []bool{true, false} {
		t.Run(fmt.Sprintf("history_available_%t", historyAvailable), func(t *testing.T) {
			origin, other, conversationID := uuid.New(), uuid.New(), uuid.New()
			store := &records{
				candidate: integrations.Integration{ID: origin, Type: 99, Name: "origin"},
				origin:    &investigation.ConversationOrigin{IntegrationID: origin, Channel: "C1", Thread: "T1"},
			}
			if !historyAvailable {
				store.briefErr = errors.New("optional history unavailable")
			}
			sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
			if err != nil {
				t.Fatal(err)
			}
			store.candidate.CredentialSealed, err = sealer.Seal("secret", integrations.CredentialBinding(origin))
			if err != nil {
				t.Fatal(err)
			}
			storeCandidates := []integrations.Integration{
				store.candidate, {ID: other, Type: 99, Name: "other"},
			}
			reads := 0
			read := func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
				reads++
				if request.Integration.ID != origin || request.OriginChannel != "C1" || request.OriginThread != "T1" {
					t.Fatalf("runtime origin changed: integration=%v channel=%q thread=%q", request.Integration.ID, request.OriginChannel, request.OriginThread)
				}
				return integrations.ToolResult{Summary: "origin thread", Content: "origin thread"}, nil
			}
			catalog, err := integrations.NewCatalog(integrations.Definition{
				Manifest: integrations.Manifest{ID: 99, Key: "chat", Name: "Chat", Category: integrations.CategoryCollaboration, Available: true, Tools: []integrations.Tool{
					{Name: "chat.thread", Description: "thread", WhenToUse: "origin", WhenNotToUse: "elsewhere", Permissions: "read", Output: "messages", SupportsThreadScope: true, Run: read},
					{Name: "chat.channel", Description: "channel", WhenToUse: "broad", WhenNotToUse: "mentions", Permissions: "read", Output: "messages", Run: read},
				}},
				Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
					return integrations.Verification{Status: integrations.StatusActive}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			// This adapter overrides only candidate discovery so Run sees both installations.
			store.candidate = storeCandidates[0]
			store.messages = []investigation.AssignedMessage{{Sequence: 1, Text: "Read this thread."}}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if len(prompt.Tools) != 3 || prompt.Tools[0].Name != "chat.thread" ||
					prompt.Tools[1].Name != UpdateHypothesesToolName || prompt.Tools[2].Name != ConcludeToolName {
					t.Fatalf("offered tools = %+v", prompt.Tools)
				}
				if call == 1 {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "thread", Name: "chat.thread",
						Arguments: json.RawMessage(`{"purpose":"read the originating thread","input":{}}`)}}}, nil
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
					ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1}),
				}}}, nil
			}}
			// records normally returns one candidate; use a small wrapper for this boundary case.
			scopedStore := &candidateRecords{records: store, candidates: storeCandidates}
			agent := configuredTestAgent(t, store, model, catalog)
			agent.Store = scopedStore
			agent.Sealer = sealer
			organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
			if err := agent.Run(context.Background(), organization, investigation.Investigation{
				ID: uuid.New(), ConversationID: conversationID, Subject: "question",
			}); err != nil {
				t.Fatal(err)
			}
			if reads != 1 || len(store.runs) != 1 || store.runs[0].Outcome != investigation.RunSucceeded {
				t.Fatalf("scoped runtime read was not preserved: reads=%d runs=%+v", reads, store.runs)
			}
		})
	}
}

type candidateRecords struct {
	*records
	candidates []integrations.Integration
}

func (r *candidateRecords) InvestigationCandidates(context.Context, tenancy.Organization) ([]integrations.Integration, error) {
	return r.candidates, nil
}

func TestRunAuditsCredentialBeforeUsingIt(t *testing.T) {
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	candidate := integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}
	candidate.CredentialSealed, err = sealer.Seal("secret", integrations.CredentialBinding(candidate.ID))
	if err != nil {
		t.Fatal(err)
	}
	store := &records{candidate: candidate}
	catalog := testCatalog(t, func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
		store.mu.Lock()
		audited := store.unseals == 1
		store.mu.Unlock()
		if !audited || request.Credential != "secret" {
			t.Fatal("credential used before its audit record")
		}
		return integrations.ToolResult{Summary: "v2", Content: "v2"}, nil
	})
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "c1", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1})}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	agent.Sealer = sealer
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "deployment"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsWhenCredentialAccessCannotBeAudited(t *testing.T) {
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	candidate := integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}
	candidate.CredentialSealed, err = sealer.Seal("secret", integrations.CredentialBinding(candidate.ID))
	if err != nil {
		t.Fatal(err)
	}
	store := &records{candidate: candidate, auditErr: errors.New("audit unavailable")}
	catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		store.toolUsed = true
		return integrations.ToolResult{}, nil
	})
	model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "c1", Name: "stub.read",
			Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	agent.Sealer = sealer
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{
		ID: uuid.New(), Subject: "deployment", WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || store.toolUsed || model.calls != 1 {
		t.Fatalf("status=%s tool_used=%t model_calls=%d", store.status, store.toolUsed, model.calls)
	}
	if !strings.Contains(store.failure, "credential access could not be audited") {
		t.Fatalf("failure = %q", store.failure)
	}
}

func TestRunRetriesMalformedConclusionOnce(t *testing.T) {
	store := &records{}
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse,
				Usage:     TokenUsage{Input: Counted(5), Output: Counted(2)},
				ToolCalls: []CompletionCall{{ID: "bad", Name: ConcludeToolName, Arguments: json.RawMessage(`{"status":"wrong"}`)}}}, nil
		}
		return Completion{Stop: StopToolUse,
			Usage:     TokenUsage{Input: Counted(7), Output: Counted(3)},
			ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 || store.status != investigation.StatusConcluded {
		t.Fatalf("calls=%d status=%s", model.calls, store.status)
	}
	if store.usage != (investigation.Usage{InputTokens: 12, OutputTokens: 5}) {
		t.Fatalf("usage=%+v", store.usage)
	}
}

func TestRunDurablyRecordsRefusalAndTruncation(t *testing.T) {
	for _, test := range []struct {
		name        string
		stop        Stop
		wantCalls   int
		wantFailure string
	}{
		{name: "refusal", stop: StopRefused, wantCalls: 1},
		{name: "truncation", stop: StopTruncated, wantCalls: 2,
			wantFailure: "truncated or carried no usable call twice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &records{}
			model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
				return Completion{Stop: test.stop, Usage: TokenUsage{
					Input: Counted(7), Output: Counted(3),
				}}, nil
			}}
			agent := configuredTestAgent(t, store, model, testCatalog(t,
				func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{}, nil
				}))
			organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
			if err := agent.Run(context.Background(), organization,
				investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
				t.Fatal(err)
			}
			if store.status != investigation.StatusFailed || model.calls != test.wantCalls ||
				store.usage.InputTokens != int64(7*test.wantCalls) ||
				store.usage.OutputTokens != int64(3*test.wantCalls) ||
				test.wantFailure != "" && !strings.Contains(store.failure, test.wantFailure) {
				t.Fatalf("status=%s calls=%d usage=%+v", store.status, model.calls, store.usage)
			}
		})
	}
}

func TestRunRejectsInvalidCitationsAtTheAgentBoundary(t *testing.T) {
	store := &records{}
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1}),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || model.calls != 2 {
		t.Fatalf("status=%s calls=%d", store.status, model.calls)
	}
}

func TestRunRejectsStateChangingActionWithoutApproval(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	unsafe, err := json.Marshal(map[string]any{
		"status": "answer_only", "summary": "Fix it.",
		"impact":   map[string]any{"status": "unknown", "current_state": "unknown", "affected_services": []string{}, "affected_users": []string{}, "summary": "unknown", "run_refs": []int{}},
		"findings": []any{}, "hypotheses": []any{}, "limitations": []any{},
		"actions": []map[string]any{{"title": "Apply fix", "type": "fix", "rationale": "restore service", "risk": "medium", "reversible": true, "requires_approval": false, "verification": "check health", "run_refs": []int{1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "read", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read it","input":{}}`),
			}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: unsafe,
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{Summary: "value"}, nil
		}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || model.calls != 3 {
		t.Fatalf("status=%s calls=%d", store.status, model.calls)
	}
}

func TestRunReturnsAnErrorWhenTheTerminalRecordCannotBeWritten(t *testing.T) {
	store := &records{terminalErr: errors.New("database unavailable")}
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("Run error = %v, want terminal persistence error", err)
	}
}

func TestRunRecordsRefusedAndDuplicateCallsWithoutRepeatingARead(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	executed := 0
	catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		return integrations.ToolResult{Summary: "value", Content: "value"}, nil
	})
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{
				{ID: "missing-purpose", Name: "stub.read", Arguments: json.RawMessage(`{"input":{}}`)},
				{ID: "read", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read it","input":{"key":"a"}}`)},
				{ID: "duplicate", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read it","input":{"key":"a"}}`)},
				{ID: "unknown", Name: "invented.read", Arguments: json.RawMessage(`{"purpose":"guess","input":{}}`)},
			}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if executed != 1 || len(store.runs) != 4 || store.status != investigation.StatusConcluded {
		t.Fatalf("executed=%d runs=%d status=%s", executed, len(store.runs), store.status)
	}
	for _, index := range []int{0, 2, 3} {
		if store.runs[index].Outcome != investigation.RunFailed {
			t.Errorf("run %d outcome=%v, want failed evidence", index+1, store.runs[index].Outcome)
		}
	}
}

func TestRunKeepsAToolFailureAsEvidenceAndStillConcludes(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, errors.New("source unavailable")
	})
	model := &scriptedModel{next: func(call int, _ Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "read", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read it","input":{}}`),
			}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || len(store.runs) != 1 ||
		store.runs[0].Outcome != investigation.RunFailed {
		t.Fatalf("status=%s runs=%+v", store.status, store.runs)
	}
}

func TestRunDurablyFailsWhenTheModelIsUnavailable(t *testing.T) {
	store := &records{}
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		return Completion{}, ErrModelUnavailable
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || store.failure == "" {
		t.Fatalf("status=%s failure=%q", store.status, store.failure)
	}
}

func TestRunForcesAnHonestConclusionAtTheTurnLimit(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "read", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read it","input":{}}`),
			}}}, nil
		}
		if prompt.ForceTool != ConcludeToolName {
			t.Fatal("turn limit did not force the conclude Tool")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{Summary: "value"}, nil
		}))
	agent.MaxTurns = 1
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.stoppedBy != investigation.StoppedByReasonerTurns {
		t.Fatalf("stopped_by=%q, want %q", store.stoppedBy, investigation.StoppedByReasonerTurns)
	}
}

func TestRunForcesConclusionBeforeTheSerializedRequestExceedsContext(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	model := &scriptedModel{
		size: func(prompt Prompt) (int, error) {
			if prompt.ForceTool == ConcludeToolName {
				return 100, nil
			}
			return 2_000, nil
		},
		next: func(_ int, prompt Prompt) (Completion, error) {
			if prompt.ForceTool != ConcludeToolName {
				t.Fatal("an oversized provider request was sent before forcing conclusion")
			}
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
			}}}, nil
		},
	}
	runner := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	runner.deployment.ContextWindowTokens = 1_500

	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := runner.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.stoppedBy != investigation.StoppedByContext || model.calls != 1 {
		t.Fatalf("stopped_by=%q calls=%d", store.stoppedBy, model.calls)
	}
}

func TestRunRetriesAContextRejectionOnlyAsAForcedConclusion(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	var rejectedTurns, rejectedContent []byte
	var rejectedOutput int64
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "read-1", Name: "stub.read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)}}}, nil
		}
		if call == 2 {
			rejectedTurns, _ = json.Marshal(prompt.Turns)
			rejectedContent, _ = json.Marshal(prompt.Content)
			rejectedOutput = prompt.MaxOutputTokens
			return Completion{}, FailedBecause(OutcomeRejected, "scripted", "test",
				"context rejected", ErrContextWindow)
		}
		if prompt.ForceTool != ConcludeToolName {
			t.Fatal("context rejection did not force the bounded recovery")
		}
		retained := append([]Turn(nil), prompt.Turns...)
		for i := range retained {
			retained[i].Instruction = ""
		}
		after, _ := json.Marshal(retained)
		if len(prompt.Turns) == 0 || !bytes.Equal(rejectedTurns, after) {
			t.Fatal("context recovery lost the completed tool exchange")
		}
		after, _ = json.Marshal(prompt.Content)
		if !bytes.Equal(rejectedContent, after) || prompt.MaxOutputTokens > rejectedOutput {
			t.Fatal("context recovery changed current input or increased output allowance")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	runner := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))

	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := runner.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 3 || store.stoppedBy != investigation.StoppedByContext {
		t.Fatalf("calls=%d stopped_by=%q", model.calls, store.stoppedBy)
	}
}

func TestRunReservesTheDeploymentOutputFromTheContextWindow(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	model := &scriptedModel{size: func(prompt Prompt) (int, error) {
		if prompt.ForceTool == ConcludeToolName {
			return 1, nil
		}
		return 3, nil
	}, next: func(_ int, prompt Prompt) (Completion, error) {
		if prompt.ForceTool != ConcludeToolName {
			t.Fatal("context window did not reserve the deployment's maximum output")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	agent.deployment.ContextWindowTokens = 1_026

	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.stoppedBy != investigation.StoppedByContext {
		t.Fatalf("stopped_by=%q, want %q", store.stoppedBy, investigation.StoppedByContext)
	}
}

func TestRunRecordsEveryReasonThatForcesAConclusion(t *testing.T) {
	tests := []struct {
		name      string
		want      string
		configure func(*Agent)
		context   func() (context.Context, context.CancelFunc)
		arguments json.RawMessage
		usage     TokenUsage
	}{
		{name: "tool budget", want: investigation.StoppedByToolRuns,
			configure: func(a *Agent) { a.MaxToolRuns = 1 }},
		{name: "stagnation", want: investigation.StoppedByStagnation,
			arguments: json.RawMessage(`{"input":{}}`)},
		{name: "wall clock", want: investigation.StoppedByWallClock,
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Minute)
			}},
		{name: "context", want: investigation.StoppedByContext,
			configure: func(a *Agent) { a.deployment.ContextWindowTokens = 1_025 }},
		{name: "context exhausted by reserved output", want: investigation.StoppedByContext,
			configure: func(a *Agent) { a.deployment.ContextWindowTokens = 1_024 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
			arguments := test.arguments
			if arguments == nil {
				arguments = json.RawMessage(`{"purpose":"read it","input":{}}`)
			}
			model := &scriptedModel{size: func(prompt Prompt) (int, error) {
				if strings.HasPrefix(test.name, "context") {
					if prompt.ForceTool == ConcludeToolName {
						return 1, nil
					}
					return 2_000, nil
				}
				return 0, nil
			}, next: func(_ int, prompt Prompt) (Completion, error) {
				if prompt.ForceTool == ConcludeToolName {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
						ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
					}}}, nil
				}
				return Completion{Stop: StopToolUse, Usage: test.usage, ToolCalls: []CompletionCall{{
					ID: "read", Name: "stub.read", Arguments: arguments,
				}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, testCatalog(t,
				func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					return integrations.ToolResult{Summary: "value"}, nil
				}))
			if test.configure != nil {
				test.configure(agent)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if test.context != nil {
				cancel()
				ctx, cancel = test.context()
			}
			defer cancel()
			organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
			if err := agent.Run(ctx, organization,
				investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
				t.Fatal(err)
			}
			if store.stoppedBy != test.want {
				t.Fatalf("stopped_by=%q, want %q", store.stoppedBy, test.want)
			}
		})
	}
}

func TestRunKeepsADurableConclusionWhenEventsFail(t *testing.T) {
	store := &records{eventErr: errors.New("event store unavailable")}
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{}, nil
		}))
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded {
		t.Fatalf("status=%s, want concluded despite event failures", store.status)
	}
}

func TestRunStopsAfterCancellationWithoutAnotherModelOrToolCall(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Type: 99, Name: "source"}}
	started := make(chan struct{})
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		close(started)
		return Completion{}, context.Canceled
	}}
	executed := 0
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			executed++
			return integrations.ToolResult{}, nil
		}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	organization, _ := tenancy.NewOrganization("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(ctx, organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("the Model was called after cancellation")
	default:
	}
	if executed != 0 || store.status != investigation.StatusFailed {
		t.Fatalf("executed=%d status=%s", executed, store.status)
	}
}

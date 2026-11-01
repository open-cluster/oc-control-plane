package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type scriptedModel struct {
	mu    sync.Mutex
	calls int
	next  func(int, Prompt) (Completion, error)
}

func (m *scriptedModel) Complete(_ context.Context, prompt Prompt) (Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.next(m.calls, prompt)
}

type records struct {
	mu              sync.Mutex
	candidate       integrations.Integration
	trigger         investigation.Trigger
	inventory       []string
	brief           investigation.Brief
	history         investigation.HistoryPage
	missingEvidence investigation.EvidenceRef
	briefErr        error
	origin          *investigation.ConversationOrigin
	originErr       error
	messages        []investigation.AssignedMessage
	messagesErr     error
	runs            []investigation.ToolRun
	runErr          error
	unseals         int
	toolUsed        bool
	conclusion      investigation.Conclusion
	status          investigation.Status
	terminalErr     error
	auditErr        error
	eventErr        error
	events          []investigation.Event
	failure         string
	stoppedBy       string
	usage           investigation.Usage
}

func (r *records) RecordToolRun(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, run investigation.ToolRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runErr != nil {
		return r.runErr
	}
	r.runs = append(r.runs, run)
	return nil
}
func (r *records) ConcludeInvestigation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, conclusion investigation.Conclusion, stoppedBy string, usage investigation.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalErr != nil {
		return r.terminalErr
	}
	r.conclusion, r.status, r.stoppedBy, r.usage = conclusion, investigation.StatusConcluded, stoppedBy, usage
	return r.appendTerminalPayload(investigation.ConcludedPayload(conclusion, stoppedBy))
}
func (r *records) FailInvestigation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, reason string, usage investigation.Usage) error {
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
func (r *records) TriggerIncident(context.Context, uuid.UUID, uuid.UUID) (investigation.Trigger, error) {
	if r.trigger.IncidentID == uuid.Nil {
		return investigation.Trigger{}, errors.New("unused")
	}
	return r.trigger, nil
}
func (r *records) InvestigationCandidates(context.Context, uuid.UUID) ([]integrations.Integration, error) {
	candidate := r.candidate
	if candidate.Provider != "" && candidate.Status == "" {
		candidate.Status = integrations.StatusVerified
		candidate.VerifiedAt = time.Now().UTC()
	}
	return []integrations.Integration{candidate}, nil
}
func (r *records) RecordCredentialUnseal(context.Context, uuid.UUID, uuid.UUID, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unseals++
	return r.auditErr
}
func (r *records) WorkloadInventory(context.Context, uuid.UUID, int) ([]string, error) {
	return r.inventory, nil
}
func (r *records) ConversationBrief(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int) (investigation.Brief, error) {
	return r.brief, r.briefErr
}

func (r *records) ConversationHistory(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) (investigation.HistoryPage, error) {
	return r.history, nil
}

func (r *records) EvidenceMissing(_ context.Context, _ uuid.UUID, refs []investigation.EvidenceRef) (bool, error) {
	for _, ref := range refs {
		if ref == r.missingEvidence {
			return true, nil
		}
	}
	return false, nil
}

func (r *records) ConversationOrigin(context.Context, uuid.UUID, uuid.UUID) (*investigation.ConversationOrigin, error) {
	return r.origin, r.originErr
}

func (r *records) InvestigationMessages(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]investigation.AssignedMessage, error) {
	return r.messages, r.messagesErr
}
func (r *records) AppendEvent(
	_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, event investigation.Event,
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
		Key: "stub", Name: "Stub", Category: integrations.CategoryAlerting, Tools: []integrations.Tool{{Name: "stub_read", Description: "Read a value to answer the question.", Run: run}},
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
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
		findings = append(findings, map[string]any{"statement": "The deployed value is v2.", "kind": "observation", "mechanism": "", "run_refs": refs, "evidence_refs": []any{}})
	}
	document := map[string]any{
		"status": "answer_only", "summary": "The deployed value is v2.",
		"impact":     map[string]any{"summary": "Impact is not established.", "run_refs": []int{}},
		"findings":   findings,
		"hypotheses": []any{}, "actions": []any{}, "limitations": []any{},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func configuredTestAgent(t *testing.T, store *records, completer Completer, catalog integrations.Catalog) *Agent {
	t.Helper()
	return &Agent{completer: completer, modelConfig: ModelConfig{Provider: "scripted", Model: "test", MaxOutputTokens: 1024}, Store: store, Catalog: catalog, Logger: slog.New(slog.DiscardHandler)}
}

func TestRunLoadsRecentReplyDetailsAndStopsRepeatedHistoryReads(t *testing.T) {
	prior := uuid.New()
	store := &records{
		messages: []investigation.AssignedMessage{{Sequence: 2, Text: "why roll back?"}},
		brief: investigation.Brief{
			HistoryBefore: 4,
			Recent: []investigation.BriefMessage{{
				Sequence:        3,
				Text:            "latency rose after deployment",
				InvestigationID: prior,
			}},
		},
		history: investigation.HistoryPage{Exchange: []investigation.BriefMessage{{
			Sequence:        3,
			Text:            "latency rose after deployment",
			InvestigationID: prior,
			Answer: &investigation.Conclusion{
				Findings: []investigation.Finding{
					{Statement: "latency increased", RunRefs: []int{1}},
					{Statement: "a prior source is no longer available", RunRefs: []int{2}},
				},
				Actions: []investigation.ActionProposal{{
					Title:        "roll back",
					Rationale:    "deployment preceded increased latency",
					Verification: "check latency",
					RunRefs:      []int{1},
				}},
			},
		}}},
	}
	store.missingEvidence = investigation.EvidenceRef{InvestigationID: prior, ToolRunOrdinal: 2}
	model := &scriptedModel{next: func(step int, prompt Prompt) (Completion, error) {
		if step == 1 {
			if !strings.Contains(prompt.Content[0].Text, "latency rose after deployment") ||
				strings.Contains(prompt.Content[0].Text, "deployment preceded increased latency") {
				t.Fatalf("initial context did not stay compact: %s", prompt.Content[0].Text)
			}
			found := false
			for _, tool := range prompt.Tools {
				if tool.Name == historyToolName {
					found = true
					if !strings.Contains(tool.Description, "4") {
						t.Errorf("recent reply cursor is not explained: %s", tool.Description)
					}
				}
			}
			if !found {
				t.Fatal("recent structured details are unavailable")
			}
		}
		if step == 2 && !strings.Contains(prompt.Turns[0].ToolResults[0].Content, "deployment preceded increased latency") {
			t.Fatalf("recent action details were not retrieved: %+v", prompt.Turns)
		}
		if step == 2 && !strings.Contains(prompt.Turns[0].ToolResults[0].Content, investigation.MissingEvidenceStatement) {
			t.Fatal("retained unavailable evidence lost its limitation")
		}
		if step < 4 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID:        fmt.Sprint(step),
				Name:      historyToolName,
				Arguments: json.RawMessage(`{"beforeSequence":4}`),
			}}}, nil
		}
		if step != 4 || !prompt.Turns[2].ToolResults[0].IsError {
			t.Fatalf("repeated history did not receive rejection feedback: step=%d", step)
		}
		var document map[string]any
		if err := json.Unmarshal(validConclusion(t, nil), &document); err != nil {
			t.Fatal(err)
		}
		document["findings"] = []map[string]any{{
			"statement":     "latency increased",
			"kind":          "observation",
			"mechanism":     "",
			"run_refs":      []int{},
			"evidence_refs": []investigation.EvidenceRef{{InvestigationID: prior, ToolRunOrdinal: 1}},
		}}
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID:        "done",
			Name:      ConcludeToolName,
			Arguments: encoded,
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, integrations.Catalog{})
	if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		Turn:           2,
		Subject:        "latency",
	}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || len(store.runs) != 0 || len(store.conclusion.Findings) != 1 ||
		len(store.conclusion.Limitations) != 1 || store.conclusion.Limitations[0].Statement != investigation.MissingEvidenceStatement {
		t.Fatalf("history did not retain prior evidence without creating new Tool Runs: %+v", store)
	}
}

func TestRunBoundsHistoryWithoutAdmittingOmittedEvidence(t *testing.T) {
	prior := uuid.New()
	large := investigation.BriefMessage{
		Sequence:        1,
		Text:            "earlier reply",
		InvestigationID: prior,
		Answer: &investigation.Conclusion{Findings: []investigation.Finding{{
			Statement: strings.Repeat("<", maxStatementLength),
			RunRefs:   []int{1},
		}, {
			Statement: strings.Repeat("&", maxStatementLength),
			RunRefs:   []int{1},
		}}},
	}
	for _, scenario := range []string{"older_reply", "single_reply", "oversized_message"} {
		t.Run(scenario, func(t *testing.T) {
			single := scenario != "older_reply"
			oversized := scenario == "oversized_message"
			entries := []investigation.BriefMessage{large}
			if oversized {
				entries[0].Text = strings.Repeat("<", maxRunContentBytes)
			}
			if !single {
				entries = append(entries, investigation.BriefMessage{Sequence: 3, Text: "latest reply"})
			}
			store := &records{
				messages: []investigation.AssignedMessage{{Sequence: 2, Text: "explain"}},
				brief:    investigation.Brief{HistoryBefore: 4},
				history: investigation.HistoryPage{
					Exchange:        entries,
					MissingEvidence: true,
					Limitations:     []string{investigation.MissingEvidenceStatement},
				},
			}
			model := &scriptedModel{next: func(step int, prompt Prompt) (Completion, error) {
				if step == 1 {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
						ID:        "history",
						Name:      historyToolName,
						Arguments: json.RawMessage(`{"beforeSequence":4}`),
					}}}, nil
				}
				content := prompt.Turns[0].ToolResults[0].Content
				if oversized {
					if !prompt.Turns[0].ToolResults[0].IsError || len(content) > maxRunContentBytes || !strings.Contains(content, "budget") {
						t.Fatalf("irreducible history response exceeded its budget: bytes=%d", len(content))
					}
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
						ID:        "done",
						Name:      ConcludeToolName,
						Arguments: validConclusion(t, nil),
					}}}, nil
				}
				_, encoded, found := strings.Cut(content, "\n")
				var page investigation.HistoryPage
				if !found || len(content) > maxRunContentBytes || json.Unmarshal([]byte(encoded), &page) != nil ||
					!page.Truncated || len(page.Exchange) != 1 || page.Exchange[0].Answer != nil {
					t.Fatalf("history feedback is unbounded or invalid: %s", content)
				}
				if page.MissingEvidence || len(page.Limitations) != 0 {
					t.Fatalf("omitted citations produced a missing-evidence warning: %+v", page)
				}
				if single {
					if page.NextBefore != 0 || !strings.Contains(page.Exchange[0].Text, "structured answer omitted") {
						t.Fatalf("oversized reply lost its omission marker or end cursor: %+v", page)
					}
				} else if page.NextBefore != 3 || page.Exchange[0].Sequence != 3 {
					t.Fatalf("truncation skipped omitted older Messages: %+v", page)
				}
				var document map[string]any
				if err := json.Unmarshal(validConclusion(t, nil), &document); err != nil {
					t.Fatal(err)
				}
				document["findings"] = []map[string]any{{
					"statement":     "not supplied",
					"kind":          "observation",
					"mechanism":     "",
					"run_refs":      []int{},
					"evidence_refs": []investigation.EvidenceRef{{InvestigationID: prior, ToolRunOrdinal: 1}},
				}}
				encodedConclusion, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
					ID:        "done",
					Name:      ConcludeToolName,
					Arguments: encodedConclusion,
				}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, integrations.Catalog{})
			if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{
				ID:             uuid.New(),
				ConversationID: uuid.New(),
				Turn:           2,
				Subject:        "history bounds",
			}); err != nil {
				t.Fatal(err)
			}
			if oversized && store.status != investigation.StatusConcluded {
				t.Fatalf("unavailable oversized history prevented an honest conclusion: %s", store.failure)
			}
			if !oversized && (store.status != investigation.StatusFailed || !strings.Contains(store.failure, "unavailable prior evidence")) {
				t.Fatalf("omitted evidence was admitted: status=%s failure=%s", store.status, store.failure)
			}
			if store.history.Exchange[0].Answer == nil {
				t.Fatal("bounding mutated the stored canonical reply")
			}
		})
	}
}

func TestRunAcceptsACustomModelWithExplicitLimits(t *testing.T) {
	store := &records{}
	model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	built, err := NewAgent(ModelConfig{
		Provider: "anthropic", Model: "claude-future-release",
		MaxOutputTokens: 1_024,
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

	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err = built.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || store.status != investigation.StatusConcluded {
		t.Fatalf("model calls=%d status=%s", model.calls, store.status)
	}
}

func TestRunDefersConclusionUntilMixedReadResultsAreRecorded(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
	catalog := testCatalog(t, func(_ context.Context, _ integrations.ToolRequest) (integrations.ToolResult, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.toolUsed = true
		return integrations.ToolResult{Summary: "v2", Content: map[string]any{"version": "v2"}}, nil
	})
	model := &scriptedModel{}
	model.next = func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, Raw: []byte(`{"opaque":"native continuation"}`), ToolCalls: []CompletionCall{
				{ID: "c1", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)},
				{ID: "premature", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1})},
			}}, nil
		}
		store.mu.Lock()
		recorded := len(store.runs) == 1 && store.toolUsed
		store.mu.Unlock()
		if !recorded {
			t.Fatal("the Tool Run was not durable before the next Model call")
		}
		if len(prompt.Turns) != 1 || len(prompt.Turns[0].ToolResults) != 2 ||
			len(prompt.Turns[0].Assistant.Calls) != 2 || string(prompt.Turns[0].Assistant.Raw) != `{"opaque":"native continuation"}` {
			t.Fatalf("mixed Tool calls lost their results or native continuation: %+v", prompt.Turns)
		}
		feedback := map[string]string{}
		for _, result := range prompt.Turns[0].ToolResults {
			feedback[result.CallID] = result.Content
		}
		if !strings.Contains(feedback["premature"], "deferred") || !strings.Contains(feedback["c1"], "[run 1]") {
			t.Fatalf("mixed Tool feedback = %v", feedback)
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1})}}}, nil
	}
	agent := configuredTestAgent(t, store, model, catalog)
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "deployment", WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || len(store.runs) != 1 {
		t.Fatalf("status=%s runs=%d", store.status, len(store.runs))
	}
	if store.events[0].Type != investigation.EventStarted || store.events[0].Payload["text"] != "Reviewing this request and available evidence sources." {
		t.Fatalf("startup activity does not explain the initial phase: %+v", store.events[0])
	}
}

func TestRunDoesNotAnnounceCompletionWhenProvenanceFails(t *testing.T) {
	store := &records{
		candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"},
		runErr:    errors.New("database write failed"),
	}
	model := &scriptedModel{next: func(round int, _ Prompt) (Completion, error) {
		if round != 1 {
			t.Fatal("an uncommitted read reached the next model decision")
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "read", Name: "stub_read", Arguments: json.RawMessage(`{"input":{}}`),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t,
		func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
			return integrations.ToolResult{Summary: "read succeeded"}, nil
		}))
	if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{ID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || len(store.runs) != 0 {
		t.Fatalf("status=%s runs=%d", store.status, len(store.runs))
	}
	for _, event := range store.events {
		if event.Type == investigation.EventToolCompleted {
			t.Fatal("completion claimed an uncommitted Tool Run")
		}
	}
}

func TestModelChoosesReadsBeforeWorkloadLabelsCauseExternalAccess(t *testing.T) {
	integrationID, incidentID := uuid.New(), uuid.New()
	store := &records{
		candidate: integrations.Integration{ID: integrationID, Provider: "kubernetes", Name: "cluster"},
		trigger: investigation.Trigger{IncidentID: incidentID, GeneratorURL: "https://alerts.example.test/alert/1", Labels: map[string]string{
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
		{Name: "kubernetes_workload_runtime", Description: "Read runtime state.", Run: run},
		{Name: "kubernetes_namespace_events", Description: "Read namespace events.", Run: run},
		{Name: "kubernetes_pod_logs", Description: "Read pod logs for diagnosis.", Run: run},
	}
	catalog, err := integrations.NewCatalog(integrations.Definition{
		Key: "kubernetes", Name: "Kubernetes", Category: integrations.CategoryInfrastructure, Tools: tools,
		Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
			return integrations.Verification{Status: integrations.StatusVerified}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			if !strings.Contains(prompt.Content[0].Text, "alert source URL: https://alerts.example.test/alert/1") {
				t.Fatal("the alert URL is not accurately labeled")
			}
			for _, expected := range []string{"kubernetes_workload_runtime", "kubernetes_namespace_events", "kubernetes_pod_logs"} {
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
				ID: "logs", Name: "kubernetes_pod_logs", Arguments: json.RawMessage(`{"purpose":"read logs","input":{"namespace":"payments"}}`),
			}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1}),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{
		ID: uuid.New(), IncidentID: incidentID, Subject: "checkout-api",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.runs) != 1 || store.runs[0].Tool != "kubernetes_pod_logs" {
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
				candidate: integrations.Integration{ID: origin, Provider: "chat", Name: "origin"},
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
				store.candidate, {ID: other, Provider: "chat", Name: "other"},
			}
			for index := range storeCandidates {
				storeCandidates[index].Status = integrations.StatusVerified
				storeCandidates[index].VerifiedAt = time.Now().UTC()
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
				Key: "chat", Name: "Chat", Category: integrations.CategoryCollaboration, Tools: []integrations.Tool{
					{Name: "chat_thread", Description: "Read the originating thread.", SupportsThreadScope: true, Run: read},
					{Name: "chat_channel", Description: "Read a channel.", Run: read},
				},
				Probe: func(context.Context, integrations.ProbeInput) integrations.Verification {
					return integrations.Verification{Status: integrations.StatusVerified}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			store.candidate = storeCandidates[0]
			store.messages = []investigation.AssignedMessage{{Sequence: 1, Text: "Read this thread."}}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if len(prompt.Tools) != 2 || prompt.Tools[0].Name != "chat_thread" ||
					prompt.Tools[1].Name != ConcludeToolName {
					t.Fatalf("offered tools = %+v", prompt.Tools)
				}
				if call == 1 {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "thread", Name: "chat_thread",
						Arguments: json.RawMessage(`{"purpose":"read the originating thread","input":{}}`)}}}, nil
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
					ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1}),
				}}}, nil
			}}
			scopedStore := &candidateRecords{records: store, candidates: storeCandidates}
			agent := configuredTestAgent(t, store, model, catalog)
			agent.Store = scopedStore
			agent.Sealer = sealer
			organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
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

func (r *candidateRecords) InvestigationCandidates(context.Context, uuid.UUID) ([]integrations.Integration, error) {
	return r.candidates, nil
}

func TestRunAuditsCredentialBeforeUsingIt(t *testing.T) {
	const credential = "fixture-credential-\"\\suffix"
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	candidate := integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}
	candidate.CredentialSealed, err = sealer.Seal(credential, integrations.CredentialBinding(candidate.ID))
	if err != nil {
		t.Fatal(err)
	}
	store := &records{candidate: candidate}
	var sourceArguments map[string]any
	catalog := testCatalog(t, func(_ context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
		store.mu.Lock()
		audited := store.unseals == 1
		store.mu.Unlock()
		if !audited || request.Credential != credential {
			t.Fatal("credential used before its audit record")
		}
		sourceArguments = request.Arguments
		return integrations.ToolResult{Summary: strings.Repeat("x", 500) + credential, Content: "v2"}, nil
	})
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			arguments, _ := json.Marshal(map[string]any{"purpose": "read deployment", "input": map[string]any{
				"password": credential, "query": "value=" + credential,
				"filters": []any{map[string]any{"api_key": credential, "namespace": "payments"}},
			}})
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{
				{ID: "c1", Name: "stub_read", Arguments: arguments},
				{ID: "rejected", Name: "unavailable", Arguments: arguments},
			}}, nil
		}
		escaped, _ := json.Marshal(credential)
		for index, run := range store.runs {
			if index == 0 && run.Summary != strings.Repeat("x", 500)+"[redacted]" {
				t.Fatal("summary clipping retained part of a known credential")
			}
			encoded, _ := json.Marshal(run.Arguments)
			feedback := prompt.Turns[0].ToolResults[index]
			if strings.Contains(string(encoded), string(escaped[1:len(escaped)-1])) ||
				strings.Contains(feedback.Content, string(escaped[1:len(escaped)-1])) ||
				strings.Contains(feedback.Content, credential) || run.Arguments["password"] != nil ||
				run.Arguments["filters"].([]any)[0].(map[string]any)["api_key"] != nil {
				t.Fatal("known credentials reached persisted arguments or prepared provider feedback")
			}
			if run.Arguments["query"] != "value=[redacted]" ||
				run.Arguments["filters"].([]any)[0].(map[string]any)["namespace"] != "payments" ||
				feedback.IsError != (index == 1) || *run.ObservationText != feedback.Content {
				t.Fatal("sanitization changed safe arguments or correlated result semantics")
			}
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, []int{1})}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	agent.Sealer = sealer
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "deployment"}); err != nil {
		t.Fatal(err)
	}
	if sourceArguments["password"] != credential || sourceArguments["query"] != "value="+credential || store.unseals != 1 {
		t.Fatal("persistence sanitization mutated execution input or opened a rejected call's credential")
	}
}

func TestRunStopsWhenCredentialAccessCannotBeAudited(t *testing.T) {
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatal(err)
	}
	candidate := integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}
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
			ID: "c1", Name: "stub_read",
			Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	agent.Sealer = sealer
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
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
	var oversized map[string]any
	if err := json.Unmarshal(validConclusion(t, nil), &oversized); err != nil {
		t.Fatal(err)
	}
	oversized["summary"] = strings.Repeat("x", 4097)
	oversizedDocument, err := json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		calls []CompletionCall
		fail  bool
	}{
		{name: "malformed", calls: []CompletionCall{{ID: "bad", Name: ConcludeToolName, Arguments: json.RawMessage(`{"status":"wrong"}`)}}},
		{name: "oversized summary", calls: []CompletionCall{{ID: "long", Name: ConcludeToolName, Arguments: oversizedDocument}}},
		{name: "duplicate conclusions", calls: []CompletionCall{
			{ID: "first", Name: ConcludeToolName, Arguments: validConclusion(t, nil)},
			{ID: "second", Name: ConcludeToolName, Arguments: validConclusion(t, nil)},
		}},
		{name: "reads while finalizing", calls: []CompletionCall{
			{ID: "read", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)},
			{ID: "premature", Name: ConcludeToolName, Arguments: validConclusion(t, nil)},
		}},
		{name: "ambiguous IDs", fail: true, calls: []CompletionCall{
			{ID: "same", Name: ConcludeToolName, Arguments: validConclusion(t, nil)},
			{ID: "same", Name: ConcludeToolName, Arguments: validConclusion(t, nil)},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &records{}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if call == 1 {
					return Completion{Stop: StopToolUse,
						Usage:     TokenUsage{Input: Counted(5), Output: Counted(2)},
						ToolCalls: test.calls, Raw: []byte(`{"opaque":"reasoning"}`)}, nil
				}
				if len(prompt.Turns) != 1 || len(prompt.Turns[0].ToolResults) != len(test.calls) ||
					string(prompt.Turns[0].Assistant.Raw) != `{"opaque":"reasoning"}` {
					t.Fatal("the malformed conclusion received no correlated correction")
				}
				for index, result := range prompt.Turns[0].ToolResults {
					if result.CallID != test.calls[index].ID || !result.IsError || result.Content == "" {
						t.Fatalf("invalid correction feedback: %+v", result)
					}
				}
				return Completion{Stop: StopToolUse,
					Usage:     TokenUsage{Input: Counted(7), Output: Counted(3)},
					ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				return integrations.ToolResult{}, nil
			}))
			organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
			if err := agent.Run(context.Background(), organization, investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
				t.Fatal(err)
			}
			if test.fail {
				if model.calls != 1 || store.status != investigation.StatusFailed {
					t.Fatalf("ambiguous calls were not rejected: calls=%d status=%s", model.calls, store.status)
				}
				return
			}
			if model.calls != 2 || store.status != investigation.StatusConcluded {
				t.Fatalf("calls=%d status=%s", model.calls, store.status)
			}
			if store.usage != (investigation.Usage{InputTokens: 12, OutputTokens: 5}) {
				t.Fatalf("usage=%+v", store.usage)
			}
			if len(store.runs) != 0 {
				t.Fatal("protocol feedback became a Tool Run")
			}
		})
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
			organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
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
	for _, test := range []struct {
		name    string
		ordinal int
		failed  bool
	}{
		{"missing", 3, false}, {"failed", 1, true}, {"suppressed", 2, false}, {"successful", 1, false}, {"unavailable prior", 1, false}, {"current as prior", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
			prior := investigation.EvidenceRef{InvestigationID: uuid.New(), ToolRunOrdinal: 1}
			id := uuid.New()
			if test.name == "current as prior" {
				prior.InvestigationID = id
			}
			if test.name == "unavailable prior" {
				store.missingEvidence = prior
				store.messages = []investigation.AssignedMessage{{Sequence: 1, Text: "question"}}
				store.brief.Findings = []investigation.PriorFinding{{InvestigationID: prior.InvestigationID, Runs: []int{1}, Statement: "A prior observation", Kind: "observation"}}
			}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if call == 1 {
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{
						{ID: "read", Name: "stub_read", Arguments: json.RawMessage(`{"input":{}}`)},
						{ID: "duplicate", Name: "stub_read", Arguments: json.RawMessage(`{"input":{}}`)},
					}}, nil
				}
				document := validConclusion(t, []int{test.ordinal})
				if test.name == "unavailable prior" || test.name == "current as prior" {
					var claimed map[string]any
					if err := json.Unmarshal(document, &claimed); err != nil {
						t.Fatal(err)
					}
					finding := claimed["findings"].([]any)[0].(map[string]any)
					finding["run_refs"], finding["evidence_refs"] = []int{}, []investigation.EvidenceRef{prior}
					document, _ = json.Marshal(claimed)
					if call == 2 {
						if err := json.Unmarshal(document, &claimed); err != nil {
							t.Fatal(err)
						}
						for _, tool := range prompt.Tools {
							if tool.Name != ConcludeToolName {
								continue
							}
							compiler := jsonschema.NewCompiler()
							if err := compiler.AddResource("conclude.json", tool.InputSchema); err != nil {
								t.Fatal(err)
							}
							schema, err := compiler.Compile("conclude.json")
							if err != nil || (schema.Validate(claimed) == nil) != (test.name == "unavailable prior") {
								t.Fatalf("provider schema does not match supplied prior evidence: %v", err)
							}
						}
					}
					if call == 3 {
						feedback := prompt.Turns[len(prompt.Turns)-1].ToolResults[0]
						if feedback.CallID != "done" || !feedback.IsError || !strings.Contains(feedback.Content, "run_refs") {
							t.Fatal("citation rejection did not explain how to cite current reads")
						}
					}
				}
				if call == 3 {
					var corrected map[string]any
					if err := json.Unmarshal(validConclusion(t, nil), &corrected); err != nil {
						t.Fatal(err)
					}
					corrected["limitations"] = []map[string]any{{"type": "missing_telemetry", "statement": "The requested evidence is unavailable.", "run_refs": []int{test.ordinal}}}
					if test.name == "missing" {
						corrected["limitations"] = []any{}
					}
					document, _ = json.Marshal(corrected)
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
					ID: "done", Name: ConcludeToolName, Arguments: document,
				}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, testCatalog(t,
				func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
					if test.failed {
						return integrations.ToolResult{}, errors.New("source unavailable")
					}
					return integrations.ToolResult{Summary: "A recorded observation"}, nil
				}))
			organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
			opened := investigation.Investigation{ID: id, Subject: "question"}
			if test.name == "unavailable prior" {
				opened.ConversationID = uuid.New()
			}
			if err := agent.Run(context.Background(), organization, opened); err != nil {
				t.Fatal(err)
			}
			wantCalls := 3
			if test.name == "successful" {
				wantCalls = 2
			}
			if store.status != investigation.StatusConcluded || model.calls != wantCalls {
				t.Fatalf("status=%s calls=%d", store.status, model.calls)
			}
			if test.name != "successful" && len(store.conclusion.Findings) != 0 {
				t.Fatal("an invalid citation established a Finding")
			}
		})
	}
}

func TestRunKeepsAToolFailureAsEvidenceAndStillConcludes(t *testing.T) {
	const secret = "sensitive-source-body"
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "investigation")
	defer parent.End()
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
	executed := 0
	catalog := testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		executed++
		if executed == 1 {
			return integrations.ToolResult{}, &url.Error{Op: "Get", URL: secret, Err: context.Canceled}
		}
		if executed == 2 {
			return integrations.ToolResult{}, context.DeadlineExceeded
		}
		return integrations.ToolResult{Summary: "source recovered"}, nil
	})
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call <= 3 {
			if prompt.ForceTool != "" {
				t.Fatal("unrelated source failures forced a premature conclusion")
			}
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "read", Name: "stub_read", Arguments: json.RawMessage(fmt.Sprintf(`{"input":{"source":%d}}`, call%2)),
			}}}, nil
		}
		return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
			ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil),
		}}}, nil
	}}
	agent := configuredTestAgent(t, store, model, catalog)
	var logs bytes.Buffer
	agent.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	id := uuid.New()
	if err := agent.Run(ctx, organization,
		investigation.Investigation{ID: id, Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || len(store.runs) != 3 ||
		store.runs[0].Outcome != investigation.RunFailed || store.runs[1].Outcome != investigation.RunFailed ||
		store.runs[2].Outcome != investigation.RunSucceeded {
		t.Fatalf("status=%s runs=%+v", store.status, store.runs)
	}
	toolLogs := 0
	for line := range strings.Lines(logs.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "Tool execution finished" {
			continue
		}
		toolLogs++
		level := "DEBUG"
		if toolLogs == 2 {
			level = "WARN"
		}
		if entry["level"] != level || entry["org_id"] != organization.String() || entry["investigation_id"] != id.String() || entry["integration_id"] != store.candidate.ID.String() || entry["tool"] != "stub_read" || entry["ordinal"] != float64(toolLogs) || entry["trace_id"] != parent.SpanContext().TraceID().String() {
			t.Errorf("Tool execution lacks safe correlation: %v", entry)
		}
		if elapsed, ok := entry["duration_ms"].(float64); !ok || elapsed < 0 || entry["duration"] != nil {
			t.Errorf("Tool duration must use numeric milliseconds: %v", entry)
		}
	}
	if toolLogs != 3 || strings.Contains(logs.String(), secret) {
		t.Fatalf("expected three safe execution logs: %s", logs.String())
	}
	if len(spans.Ended()) != 3 {
		t.Fatalf("Tool execution spans = %d, want three", len(spans.Ended()))
	}
	for _, span := range spans.Ended() {
		for _, attr := range span.Attributes() {
			if attr.Key == "oc.tool.ordinal" && attr.Value.AsInt64() == 1 && span.Status().Code == codes.Error {
				t.Error("cancelled Tool execution was reported as an error span")
			}
		}
		if span.Parent().SpanID() != parent.SpanContext().SpanID() || strings.Contains(fmt.Sprint(span.Attributes()), secret) || strings.Contains(span.Status().Description, secret) {
			t.Fatal("Tool span lost its parent or exposed source content")
		}
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
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed || store.failure == "" {
		t.Fatalf("status=%s failure=%q", store.status, store.failure)
	}
}

func TestRunForcesAnHonestConclusionAtTheTurnLimit(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
				ID: "read", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read it","input":{}}`),
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
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.stoppedBy != investigation.StoppedByReasonerTurns {
		t.Fatalf("stopped_by=%q, want %q", store.stoppedBy, investigation.StoppedByReasonerTurns)
	}
}

func TestRunPreservesCompleteContextOnNormalRequests(t *testing.T) {
	prior := uuid.New()
	store := &records{
		candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"},
		inventory: []string{"cluster/prod/payments"},
		trigger:   investigation.Trigger{IncidentID: uuid.New(), Title: "High latency"},
		messages: []investigation.AssignedMessage{
			{Sequence: 3, Author: "alice", Text: "Read eu-west-2 only."},
			{Sequence: 4, Author: "bob", Text: "Exclude eu-central-1; preserve this correction."},
		},
		brief: investigation.Brief{HistoryBefore: 3, NextBefore: 2, MissingEvidence: true,
			OpenHypotheses: []string{"Check the rollout"},
			Findings: []investigation.PriorFinding{
				{InvestigationID: prior, Statement: "KEEP CITED FINDING", Kind: "observation", Runs: []int{1}},
				{InvestigationID: prior, Statement: "KEEP CITED FINDING", Kind: "contributing_factor", Runs: []int{1}},
				{InvestigationID: prior, Statement: "KEEP CITED FINDING", Kind: "observation", Runs: []int{1}, WindowFrom: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)},
				{InvestigationID: prior, Statement: "KEEP CITED FINDING", Kind: "observation", Runs: []int{1}},
			},
			Recent: []investigation.BriefMessage{
				{Text: "OLD OPTIONAL CONTEXT"},
			}, Limitations: []string{investigation.MissingEvidenceStatement}},
	}
	model := &scriptedModel{
		next: func(_ int, prompt Prompt) (Completion, error) {
			if prompt.ForceTool != "" || prompt.MaxOutputTokens != 1_024 {
				t.Fatal("a normal request forced finalization or changed the output allowance")
			}
			text := prompt.System[1].Text + prompt.Content[0].Text
			first, second := strings.Index(text, store.messages[0].Text), strings.Index(text, store.messages[1].Text)
			if first < 0 || second <= first || !strings.Contains(text, "alice") || !strings.Contains(text, "bob") ||
				!strings.Contains(text, "TASK follow_up") || !strings.Contains(text, "beforeSequence=3") ||
				!strings.Contains(text, investigation.MissingEvidenceStatement) ||
				!strings.Contains(text, "cluster/prod/payments") || !strings.Contains(text, "High latency") ||
				!strings.Contains(text, "Check the rollout") || !strings.Contains(text, "beforeSequence=2") ||
				strings.Count(text, "KEEP CITED FINDING") != 3 ||
				!strings.Contains(text, "OLD OPTIONAL CONTEXT") {
				t.Fatalf("admission lost current input or history metadata: %s", text)
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

	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := runner.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), IncidentID: store.trigger.IncidentID, ConversationID: uuid.New(), Turn: 2, Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded || store.stoppedBy != "" || model.calls != 1 {
		t.Fatalf("stopped_by=%q calls=%d", store.stoppedBy, model.calls)
	}
}

func TestRunRetriesAContextRejectionOnlyAsAForcedConclusion(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
	var rejectedTurns, rejectedContent []byte
	var rejectedOutput int64
	model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
		if call == 1 {
			return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "read-1", Name: "stub_read", Arguments: json.RawMessage(`{"purpose":"read deployment","input":{}}`)}}}, nil
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

	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := runner.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 3 || store.stoppedBy != investigation.StoppedByContext {
		t.Fatalf("calls=%d stopped_by=%q", model.calls, store.stoppedBy)
	}
}

func TestRunBoundsContextRecoveryWithoutLosingInput(t *testing.T) {
	for _, test := range []struct {
		name        string
		optional    bool
		rejectRetry bool
		citeOmitted bool
	}{
		{name: "optional context", optional: true},
		{name: "rejected recovery", optional: true, rejectRetry: true},
		{name: "no reduction"},
		{name: "omitted evidence", optional: true, citeOmitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prior := uuid.New()
			store := &records{messages: []investigation.AssignedMessage{{Sequence: 2, Author: "alice", Text: "Keep the complete current request."}}}
			if test.optional {
				store.candidate = integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}
				store.brief = investigation.Brief{HistoryBefore: 2,
					Findings: []investigation.PriorFinding{{InvestigationID: prior, Statement: "OPTIONAL OBSERVATION", Runs: []int{1}}}}
			}
			model := &scriptedModel{next: func(call int, prompt Prompt) (Completion, error) {
				if call > 2 {
					t.Fatal("context recovery exceeded its bound")
				}
				if !strings.Contains(prompt.Content[0].Text, store.messages[0].Text) ||
					!strings.Contains(prompt.Content[0].Text, "alice") || prompt.MaxOutputTokens != 1_024 {
					t.Fatal("context recovery changed the current input or output allowance")
				}
				if call == 1 || test.rejectRetry {
					return Completion{}, ContextRejected("scripted", "test", "context rejected", nil)
				}
				if strings.Contains(prompt.Content[0].Text, "OPTIONAL OBSERVATION") || prompt.ForceTool != "" {
					t.Fatal("optional recovery retained history or forced finalization")
				}
				document := validConclusion(t, nil)
				if test.citeOmitted {
					var conclusion map[string]any
					if err := json.Unmarshal(document, &conclusion); err != nil {
						t.Fatal(err)
					}
					conclusion["findings"] = []any{map[string]any{"statement": "An old observation.", "kind": "observation", "mechanism": "", "run_refs": []int{},
						"evidence_refs": []investigation.EvidenceRef{{InvestigationID: prior, ToolRunOrdinal: 1}}}}
					document, _ = json.Marshal(conclusion)
				}
				return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: document}}}, nil
			}}
			agent := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
				t.Fatal("context recovery executed an unrequested read")
				return integrations.ToolResult{}, nil
			}))
			if err := agent.Run(context.Background(), uuid.New(), investigation.Investigation{ID: uuid.New(), ConversationID: uuid.New(), Turn: 2}); err != nil {
				t.Fatal(err)
			}
			wantCalls, wantStatus := 2, investigation.StatusConcluded
			if !test.optional {
				wantCalls = 1
			}
			if !test.optional || test.rejectRetry || test.citeOmitted {
				wantStatus = investigation.StatusFailed
			}
			if model.calls != wantCalls || store.status != wantStatus || len(store.conclusion.Limitations) != 0 {
				t.Fatalf("calls=%d status=%s failure=%s", model.calls, store.status, store.failure)
			}
			if test.rejectRetry && (!strings.Contains(store.failure, "context rejected") || store.stoppedBy != "") {
				t.Fatalf("a second context rejection lost its technical failure: %s", store.failure)
			}
		})
	}
}

func TestRunRecordsOnlyResourceCeilingsThatForceAConclusion(t *testing.T) {
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
		{name: "wall clock", want: investigation.StoppedByWallClock,
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Minute)
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
			arguments := test.arguments
			if arguments == nil {
				arguments = json.RawMessage(`{"purpose":"read it","input":{}}`)
			}
			model := &scriptedModel{next: func(_ int, prompt Prompt) (Completion, error) {
				if prompt.ForceTool == ConcludeToolName {
					conclusion := validConclusion(t, nil)
					return Completion{Stop: StopToolUse, ToolCalls: []CompletionCall{{
						ID: "done", Name: ConcludeToolName, Arguments: conclusion,
					}}}, nil
				}
				return Completion{Stop: StopToolUse, Usage: test.usage, ToolCalls: []CompletionCall{{
					ID: "read", Name: "stub_read", Arguments: arguments,
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
			organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
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
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	if err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusConcluded {
		t.Fatalf("status=%s, want concluded despite event failures", store.status)
	}
}

func TestRunStopsAfterCancellationWithoutAnotherModelOrToolCall(t *testing.T) {
	store := &records{candidate: integrations.Integration{ID: uuid.New(), Provider: "stub", Name: "source"}}
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
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
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

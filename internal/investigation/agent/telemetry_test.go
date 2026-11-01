package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

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
	var logs bytes.Buffer
	agent.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	err := agent.Run(context.Background(), organization,
		investigation.Investigation{ID: uuid.New(), Subject: "question"})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("Run error = %v, want terminal persistence error", err)
	}
	if strings.Contains(logs.String(), "Investigation concluded") || strings.Contains(logs.String(), "Investigation failed") {
		t.Fatalf("uncommitted outcome was logged: %s", logs.String())
	}
}

func TestRunFailureLogsAreSafeAndDescribeCommittedOutcome(t *testing.T) {
	const secret = "provider-body-secret-must-not-be-logged"
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	store := &records{}
	model := &scriptedModel{next: func(int, Prompt) (Completion, error) {
		return Completion{RequestID: "provider-123", Model: "responding-model", Raw: []byte(secret), Document: []byte(secret)}, FailedBecause(OutcomeOutage, "scripted", "test", secret, errors.New(secret))
	}}
	agent := configuredTestAgent(t, store, model, testCatalog(t, func(context.Context, integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	}))
	var logs bytes.Buffer
	agent.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	agent.Instrument(NewTelemetry(agent.Logger))
	org, id := uuid.New(), uuid.New()
	if err := agent.Run(context.Background(), org, investigation.Investigation{ID: id, Subject: secret}); err != nil {
		t.Fatal(err)
	}
	if store.status != investigation.StatusFailed {
		t.Fatalf("status = %s", store.status)
	}
	text := logs.String()
	if strings.Contains(text, secret) {
		t.Fatalf("sensitive provider content escaped: %s", text)
	}
	for _, span := range spans.Ended() {
		if strings.Contains(fmt.Sprint(span.Attributes()), secret) || strings.Contains(span.Status().Description, secret) {
			t.Fatal("sensitive provider content escaped through tracing")
		}
	}
	if strings.Count(text, `"msg":"Investigation failed"`) != 1 || !strings.Contains(text, org.String()) || !strings.Contains(text, id.String()) || !strings.Contains(text, `"outcome":"outage"`) || !strings.Contains(text, `"provider_request_id":"provider-123"`) ||
		!strings.Contains(text, `"model_answered":"responding-model"`) {
		t.Fatalf("committed failure lacks safe correlation: %s", text)
	}
	logs.Reset()
	model.next = func(int, Prompt) (Completion, error) {
		return Completion{Model: "responding-model", Stop: StopToolUse,
			Usage:     TokenUsage{Input: Counted(34), CacheRead: Counted(20), CacheWrite: Counted(5), Output: Counted(0)},
			ToolCalls: []CompletionCall{{ID: "done", Name: ConcludeToolName, Arguments: validConclusion(t, nil)}}}, nil
	}
	if err := agent.Run(context.Background(), org, investigation.Investigation{ID: uuid.New(), Subject: "question"}); err != nil {
		t.Fatal(err)
	}
	text = logs.String()
	if !strings.Contains(text, `"model_answered":"responding-model"`) ||
		!strings.Contains(text, `"output_tokens":0`) || strings.Contains(text, `"reasoning_tokens"`) ||
		store.usage != (investigation.Usage{InputTokens: 59, OutputTokens: 0}) {
		t.Fatalf("reported identity or usage was lost: usage=%+v logs=%s", store.usage, text)
	}
}

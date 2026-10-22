package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	observability "github.com/open-cluster/oc-control-plane/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const meterName = "github.com/open-cluster/oc-control-plane/internal/investigation/agent"

type Telemetry struct {
	logger  *slog.Logger
	tracer  trace.Tracer
	calls   metric.Int64Counter
	tokens  metric.Int64Counter
	latency metric.Float64Histogram
}

func NewTelemetry(logger *slog.Logger) *Telemetry {
	meter := otel.Meter(meterName)
	built := &Telemetry{
		logger: logger,
		tracer: otel.Tracer(meterName),
	}

	var err error
	if built.calls, err = meter.Int64Counter("oc.reasoning.calls",
		metric.WithDescription("Model calls attempted."),
		metric.WithUnit("{call}")); err != nil {
		logger.Warn("reasoning call metric unavailable", slog.String("error", err.Error()))
	}
	if built.tokens, err = meter.Int64Counter("oc.reasoning.tokens",
		metric.WithDescription("Tokens consumed by reasoner calls, by kind."),
		metric.WithUnit("{token}")); err != nil {
		logger.Warn("reasoning token metric unavailable", slog.String("error", err.Error()))
	}
	if built.latency, err = meter.Float64Histogram("oc.reasoning.call_duration",
		metric.WithDescription("Wall-clock duration of one provider call."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("reasoning latency metric unavailable", slog.String("error", err.Error()))
	}
	return built
}

func (t *Telemetry) complete(
	ctx context.Context, completer Completer, config ModelConfig, prompt Prompt,
) (Completion, error) {
	if t == nil {
		return completer.Complete(ctx, prompt)
	}

	ctx, span := t.tracer.Start(ctx, "reasoning.complete", trace.WithAttributes(
		attribute.String("oc.reasoning.provider", config.Provider),
		attribute.String("oc.reasoning.model", config.Model),
	))
	defer span.End()

	started := time.Now()
	completion, err := completer.Complete(ctx, prompt)
	elapsed := time.Since(started)
	model := config.Model
	if completion.Model != "" {
		model = completion.Model
		span.SetAttributes(attribute.String("oc.reasoning.model_answered", model))
	}
	measured := metric.WithAttributes(
		attribute.String("provider", config.Provider),
		attribute.String("model", model),
	)

	span.SetAttributes(attribute.String("oc.reasoning.stop", completion.Stop.String()))
	requestID := safeProviderRequestID(completion.RequestID)
	if requestID != "" {
		span.SetAttributes(attribute.String("oc.reasoning.provider_request_id", requestID))
	}

	if t.latency != nil {
		t.latency.Record(ctx, elapsed.Seconds(), measured)
	}
	if t.calls != nil {
		t.calls.Add(ctx, 1, measured)
	}
	entry := []any{
		slog.String("provider", config.Provider),
		slog.String("model", config.Model),
		slog.String("stop", completion.Stop.String()),
		slog.Duration("duration", elapsed),
	}
	for kind, count := range map[string]Count{
		"input": completion.Usage.Input, "output": completion.Usage.Output,
		"cache_write": completion.Usage.CacheWrite, "cache_read": completion.Usage.CacheRead,
		"reasoning": completion.Usage.Reasoning,
	} {
		if count.Reported {
			entry = append(entry, slog.Int64(kind+"_tokens", count.Tokens))
			if t.tokens != nil {
				t.tokens.Add(ctx, count.Tokens, measured,
					metric.WithAttributes(attribute.String("kind", kind)))
			}
		}
	}
	if completion.Model != "" {
		entry = append(entry, slog.String("model_answered", model))
	}
	if requestID != "" {
		entry = append(entry, slog.String("provider_request_id", requestID))
	}
	logger := observability.LoggerFor(ctx, t.logger, "")
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			span.SetStatus(codes.Error, "the provider call failed")
		}
		outcome := failureClass(err)
		span.SetAttributes(attribute.String("oc.reasoning.outcome", outcome))
		entry = append(entry, slog.String("outcome", outcome), slog.String("error", "model call failed"))
		if errors.Is(err, context.Canceled) {
			logger.DebugContext(ctx, "model call cancelled", entry...)
		} else {
			logger.WarnContext(ctx, "model call failed", entry...)
		}
		return completion, err
	}
	logger.DebugContext(ctx, "model call completed", entry...)
	return completion, err
}

func failureClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if outcome, known := OutcomeOf(err); known {
		return outcome.String()
	}
	return "unknown"
}

func safeProviderRequestID(id string) string {
	if len(id) > 256 {
		return ""
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return id
}

func (r *Agent) logConclusion(ctx context.Context, state *runState, conclusion investigation.Conclusion, stoppedBy string) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("oc.investigation.status", "concluded"),
		attribute.String("oc.investigation.conclusion_status", string(conclusion.Status)))
	entry := []any{
		slog.String("conclusion_status", string(conclusion.Status)),
		slog.Duration("duration", time.Since(state.startedAt)),
		slog.Int("turns", state.turns), slog.Int("tool_runs", state.executed),
		slog.Int64("input_tokens", state.usage.InputTokens), slog.Int64("output_tokens", state.usage.OutputTokens),
	}
	if stoppedBy != "" {
		entry = append(entry, slog.String("stopped_by", stoppedBy))
		span.SetAttributes(attribute.String("oc.investigation.stopped_by", stoppedBy))
	}
	r.Logger.InfoContext(ctx, "investigation concluded", entry...)
}

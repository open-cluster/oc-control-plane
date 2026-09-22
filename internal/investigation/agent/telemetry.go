package agent

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// meterName identifies this surface's instruments and spans: the package path, so a
// metric found in a dashboard leads back to the code that emits it.
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

// complete runs one provider call inside its span and emits the call's telemetry.
func (t *Telemetry) complete(
	ctx context.Context, provider Completer, config ModelConfig, prompt Prompt,
) (Completion, error) {
	if t == nil {
		return provider.Complete(ctx, prompt)
	}

	// The configured provider and model: bounded, this process's own strings. The
	// model that ANSWERED goes on the span and the log line, never on a metric.
	measured := metric.WithAttributes(
		attribute.String("provider", config.Provider),
		attribute.String("model", config.Model),
	)

	ctx, span := t.tracer.Start(ctx, "reasoning.complete", trace.WithAttributes(
		attribute.String("oc.reasoning.provider", config.Provider),
		attribute.String("oc.reasoning.model", config.Model),
	))
	defer span.End()

	started := time.Now()
	completion, err := provider.Complete(ctx, prompt)
	elapsed := time.Since(started)

	span.SetAttributes(
		attribute.String("oc.reasoning.model_answered", completion.Model),
		attribute.String("oc.reasoning.request_id", completion.RequestID),
		attribute.String("oc.reasoning.stop", completion.Stop.String()),
	)

	if t.latency != nil {
		t.latency.Record(ctx, elapsed.Seconds(), measured)
	}
	if t.calls != nil {
		t.calls.Add(ctx, 1, measured)
	}
	if t.tokens != nil {
		for kind, count := range map[string]Count{
			"input":       completion.Usage.Input,
			"output":      completion.Usage.Output,
			"cache_write": completion.Usage.CacheWrite,
			"cache_read":  completion.Usage.CacheRead,
			"reasoning":   completion.Usage.Reasoning,
		} {
			// An unreported figure adds nothing: zero is a measurement, absent is not.
			if count.Reported {
				t.tokens.Add(ctx, count.Tokens, measured,
					metric.WithAttributes(attribute.String("kind", kind)))
			}
		}
	}

	entry := []any{
		slog.String("provider", config.Provider),
		slog.String("model", config.Model),
		slog.String("model_answered", completion.Model),
		slog.String("request_id", completion.RequestID),
		slog.String("stop", completion.Stop.String()),
		slog.Duration("latency", elapsed),
		slog.Int64("input_tokens", completion.Usage.Input.Or(0)),
		slog.Int64("output_tokens", completion.Usage.Output.Or(0)),
		slog.Int64("cache_read_tokens", completion.Usage.CacheRead.Or(0)),
		slog.Int64("cache_write_tokens", completion.Usage.CacheWrite.Or(0)),
		slog.Int64("reasoning_tokens", completion.Usage.Reasoning.Or(0)),
	}
	if err != nil {
		span.SetStatus(codes.Error, "the provider call failed")
		if outcome, named := OutcomeOf(err); named {
			outcomeWord := outcome.String()
			span.SetAttributes(attribute.String("oc.reasoning.outcome", outcomeWord))
			entry = append(entry, slog.String("outcome", outcomeWord))
		}
		t.logger.WarnContext(ctx, "reasoner call failed", entry...)
		return completion, err
	}
	t.logger.InfoContext(ctx, "reasoner call", entry...)
	return completion, err
}

package reasoning

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

// PER-CALL REASONING TELEMETRY, AND WHY IT IS NOT A TABLE.
//
// Everything a debugging engineer wants per model call — provider, the model that
// answered, request identifier, stop reason, latency, token decomposition, spend, the
// agent revision — is telemetry with no product consumer, so it lands in the existing
// observability stack: one span and one structured log line per Provider.Complete,
// plus low-cardinality instruments. The one per-investigation product fact, stopped_by,
// is a column; nothing else is.
//
// NO ORGANIZATION LABEL ON ANY INSTRUMENT — tenant identity belongs on spans, and the
// reasoning boundary does not even carry one. Metric attributes are the configured
// provider and model, this build's own bounded strings, never anything a response said.

// meterName identifies this surface's instruments and spans: the package path, so a
// metric found in a dashboard leads back to the code that emits it.
const meterName = "github.com/open-cluster/oc-control-plane/internal/reasoning"

// Telemetry emits the per-call signal. Built once at startup with the derived agent
// revision; a nil *Telemetry disables observation without disabling reasoning.
type Telemetry struct {
	logger   *slog.Logger
	tracer   trace.Tracer
	revision string

	tokens  metric.Int64Counter
	spend   metric.Int64Counter
	latency metric.Float64Histogram
}

// NewTelemetry builds the instruments. A failure to construct one is logged and leaves
// it nil — every emit tolerates that, because telemetry that refuses to start would
// take investigations with it.
func NewTelemetry(logger *slog.Logger, agentRevision string) *Telemetry {
	meter := otel.Meter(meterName)
	built := &Telemetry{
		logger:   logger,
		tracer:   otel.Tracer(meterName),
		revision: agentRevision,
	}

	var err error
	if built.tokens, err = meter.Int64Counter("oc.reasoning.tokens",
		metric.WithDescription("Tokens consumed by reasoner calls, by kind."),
		metric.WithUnit("{token}")); err != nil {
		logger.Warn("reasoning token metric unavailable", slog.String("error", err.Error()))
	}
	if built.spend, err = meter.Int64Counter("oc.reasoning.spend",
		metric.WithDescription("Integer micro-cents spent on reasoner calls."),
		metric.WithUnit("{microcent}")); err != nil {
		logger.Warn("reasoning spend metric unavailable", slog.String("error", err.Error()))
	}
	if built.latency, err = meter.Float64Histogram("oc.reasoning.call_duration",
		metric.WithDescription("Wall-clock duration of one provider call."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("reasoning latency metric unavailable", slog.String("error", err.Error()))
	}
	return built
}

// AgentRevision reports the revision this telemetry stamps on every call.
func (t *Telemetry) AgentRevision() string {
	if t == nil {
		return ""
	}
	return t.revision
}

// complete runs one provider call inside its span and emits the call's telemetry. It is
// the one wrapper around Provider.Complete, so no call can happen unobserved.
func (t *Telemetry) complete(
	ctx context.Context, provider Provider, deployment Deployment, rate Rate,
	prompt Prompt,
) (Completion, error) {
	if t == nil {
		return provider.Complete(ctx, prompt)
	}

	// The configured provider and model: bounded, this deployment's own strings. The
	// model that ANSWERED goes on the span and the log line, never on a metric.
	measured := metric.WithAttributes(
		attribute.String("provider", deployment.Provider),
		attribute.String("model", deployment.Model),
	)

	ctx, span := t.tracer.Start(ctx, "reasoning.complete", trace.WithAttributes(
		attribute.String("oc.reasoning.provider", deployment.Provider),
		attribute.String("oc.reasoning.model", deployment.Model),
		attribute.String("oc.reasoning.agent_revision", t.revision),
	))
	defer span.End()

	started := time.Now()
	completion, err := provider.Complete(ctx, prompt)
	elapsed := time.Since(started)

	cost := rate.Cost(completion.Usage)
	span.SetAttributes(
		attribute.String("oc.reasoning.model_answered", completion.Model),
		attribute.String("oc.reasoning.request_id", completion.RequestID),
		attribute.String("oc.reasoning.stop", completion.Stop.String()),
		attribute.Int64("oc.reasoning.spend_microcents", cost),
	)

	if t.latency != nil {
		t.latency.Record(ctx, elapsed.Seconds(), measured)
	}
	if t.spend != nil {
		t.spend.Add(ctx, cost, measured)
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
		slog.String("provider", deployment.Provider),
		slog.String("model", deployment.Model),
		slog.String("model_answered", completion.Model),
		slog.String("request_id", completion.RequestID),
		slog.String("stop", completion.Stop.String()),
		slog.Duration("latency", elapsed),
		slog.Int64("input_tokens", completion.Usage.Input.Or(0)),
		slog.Int64("output_tokens", completion.Usage.Output.Or(0)),
		slog.Int64("cache_read_tokens", completion.Usage.CacheRead.Or(0)),
		slog.Int64("cache_write_tokens", completion.Usage.CacheWrite.Or(0)),
		slog.Int64("reasoning_tokens", completion.Usage.Reasoning.Or(0)),
		slog.Int64("spend_microcents", cost),
		slog.String("agent_revision", t.revision),
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

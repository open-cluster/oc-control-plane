package slack

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MessageInstruments records the bounded Slack Message worker lifecycle.
type MessageInstruments struct {
	outcomes metric.Int64Counter
	delay    metric.Float64Histogram
}

// NewMessageInstruments retains the public metric names and scope used by existing dashboards.
func NewMessageInstruments(logger *slog.Logger) MessageInstruments {
	if logger == nil {
		logger = slog.Default()
	}
	meter := otel.Meter("github.com/open-cluster/oc-control-plane/internal/webhooks")
	counter, err := meter.Int64Counter("oc.webhooks.deliveries",
		metric.WithDescription("Durable webhook delivery lifecycle transitions."),
		metric.WithUnit("{delivery}"))
	if err != nil {
		logger.Warn("slack message metric unavailable", slog.String("error", err.Error()))
	}
	delay, err := meter.Float64Histogram("oc.webhooks.delivery_delay",
		metric.WithDescription("Time between durable Slack message acceptance and worker claim."),
		metric.WithUnit("s"))
	if err != nil {
		logger.Warn("slack message delay metric unavailable", slog.String("error", err.Error()))
	}
	return MessageInstruments{outcomes: counter, delay: delay}
}

// Count records one supported Slack Message worker outcome.
func (i MessageInstruments) Count(ctx context.Context, outcome string) {
	if outcome != "delayed" && outcome != "failed" {
		return
	}
	if i.outcomes != nil {
		i.outcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// ObserveDelay records the time from Slack Message acceptance to worker claim.
func (i MessageInstruments) ObserveDelay(ctx context.Context, elapsed time.Duration) {
	if i.delay != nil {
		i.delay.Record(ctx, max(elapsed, 0).Seconds())
	}
}

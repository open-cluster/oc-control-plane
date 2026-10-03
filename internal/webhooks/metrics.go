package webhooks

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/open-cluster/oc-control-plane/internal/webhooks"

const (
	surfaceAlert = "alert"
	surfaceSlack = "slack"

	resultAccepted    = "accepted"
	resultDuplicate   = "duplicate"
	resultRejected    = "rejected"
	resultError       = "error"
	resultRateLimited = "rate_limited"
)

type instruments struct {
	requests             metric.Int64Counter
	alertEvents          metric.Int64Counter
	slackAcknowledgement metric.Float64Histogram
}

func newInstruments(logger *slog.Logger) instruments {
	if logger == nil {
		logger = slog.Default()
	}
	meter := otel.Meter(meterName)
	var built instruments
	var err error
	if built.requests, err = meter.Int64Counter("oc.webhooks.requests",
		metric.WithDescription("Webhook requests by surface and result."),
		metric.WithUnit("{request}")); err != nil {
		logger.Warn("webhook request metric unavailable", slog.String("error", err.Error()))
	}
	if built.alertEvents, err = meter.Int64Counter("oc.webhooks.alert_events",
		metric.WithDescription("Alert Events committed from accepted webhook requests."),
		metric.WithUnit("{alert_event}")); err != nil {
		logger.Warn("webhook Alert Event metric unavailable", slog.String("error", err.Error()))
	}
	if built.slackAcknowledgement, err = meter.Float64Histogram("oc.webhooks.slack_ack_duration",
		metric.WithDescription("Time to acknowledge one Slack webhook request."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("Slack acknowledgement metric unavailable", slog.String("error", err.Error()))
	}
	return built
}

func (i instruments) countRequest(ctx context.Context, surface, result string) {
	if i.requests == nil || !validSurface(surface) || !validResult(result) {
		return
	}
	i.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("surface", surface),
		attribute.String("result", result),
	))
}

func (i instruments) countAlertEvents(ctx context.Context, recorded int) {
	if i.alertEvents != nil && recorded > 0 {
		i.alertEvents.Add(ctx, int64(recorded))
	}
}

func (i instruments) observeSlackAcknowledgement(ctx context.Context, elapsed time.Duration) {
	if i.slackAcknowledgement != nil {
		i.slackAcknowledgement.Record(ctx, max(elapsed, 0).Seconds())
	}
}

func validSurface(surface string) bool {
	return surface == surfaceAlert || surface == surfaceSlack
}

func validResult(result string) bool {
	switch result {
	case resultAccepted, resultDuplicate, resultRejected, resultError, resultRateLimited:
		return true
	default:
		return false
	}
}

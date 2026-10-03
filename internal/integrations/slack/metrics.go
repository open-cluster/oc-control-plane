package slack

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/open-cluster/oc-control-plane/internal/integrations/slack"

type Instruments struct {
	replies metric.Int64Counter
}

const (
	replyAnswered  = "answered"
	replyRetried   = "retried"
	replyAbandoned = "abandoned"
)

const outcomeKey = "outcome"

func NewInstruments(logger *slog.Logger) Instruments {
	var built Instruments
	replies, err := otel.Meter(meterName).Int64Counter("oc.slack.replies",
		metric.WithDescription("Slack reply attempts, by what happened to them."),
		metric.WithUnit("{reply}"))
	if err != nil {
		logger.Warn("slack reply metric unavailable", slog.String("error", err.Error()))
		return built
	}
	built.replies = replies
	return built
}

func (i Instruments) countReply(ctx context.Context, outcome string) {
	if i.replies == nil {
		return
	}
	i.replies.Add(ctx, 1, metric.WithAttributes(attribute.String(outcomeKey, outcome)))
}

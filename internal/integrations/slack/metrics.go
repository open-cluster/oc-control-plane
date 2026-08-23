package slack

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// WHAT AN OPERATOR WATCHING THE CHAT SURFACE CAN SEE.
//
// A thread that never gets an answer looks identical from the outside whether the model is
// slow, Slack is refusing us, or a worker died. These are what tell them apart.
//
// NO ORGANIZATION LABEL. Tenant identity belongs on a span; at the stated scale a tenant label
// is a cardinality failure in any Prometheus-shaped backend, and the rule has a named home in
// internal/observability. What is attributed here is a closed set of this build's own words,
// never anything a vendor or a caller supplied.

const meterName = "github.com/open-cluster/oc-control-plane/internal/integrations/slack"

// Instruments are the counters the reply worker keeps. The zero value records nothing, so a
// caller that has not built them is not a caller that crashes.
type Instruments struct {
	replies metric.Int64Counter
}

// The outcomes a reply attempt is counted under. Each is a distinct operational story rather
// than a shade of "failed": retried is Slack being briefly unavailable, abandoned is a thread
// that will now never be answered and is the one worth paging on.
const (
	replyAnswered  = "answered"
	replyRetried   = "retried"
	replyAbandoned = "abandoned"
)

const outcomeKey = "outcome"

// NewInstruments builds the counters.
//
// A failure to construct one is logged and leaves the counter nil, and every emit tolerates a
// nil: telemetry that refused to start would take the answers with it, which trades an
// observability gap for a customer's question going unanswered.
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

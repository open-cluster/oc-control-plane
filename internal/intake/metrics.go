package intake

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// WHAT AN OPERATOR WATCHING INTAKE CAN SEE.
//
// A broken integration and a quiet night look identical from the outside, and the delivery history
// answers that per Connection. What it cannot answer is the shape of the whole surface: whether
// rejections are rising, whether a storm is under way, whether anybody's alert grouping is doing
// anything. That is what these are for.
//
// NO ORGANIZATION LABEL, ON ANY OF THEM. Tenant identity belongs on a span; at the stated scale of
// five thousand organizations a tenant label is a cardinality failure in any Prometheus-shaped
// backend, and the rule has a named home in internal/observability. What is attributed here is the
// DISPOSITION and the reason — a closed set of this build's own strings, never anything a caller
// supplied.
//
// Every instrument is created once per surface rather than per request, because an instrument
// rebuilt per call is a new time series per call.

// meterName identifies this surface's instruments. It is the package path so that a metric found
// in a dashboard leads back to the code that emits it.
const meterName = "github.com/open-cluster/oc-control-plane/internal/intake"

// instruments are the counters this surface keeps.
type instruments struct {
	deliveries metric.Int64Counter
	signals    metric.Int64Counter
	episodes   metric.Int64Counter
	// slackEvents is the chat surface's own arrivals. Counted apart from deliveries
	// because they answer different questions: a delivery is a customer's alerting
	// reaching us, and this is a person speaking to OpenCluster — and the dispositions that
	// matter are different ones. A rising invalid-signature count here is somebody probing
	// or an app registration that has been rotated; a rising duplicate count is Slack
	// retrying, which means our answer is not reaching it.
	slackEvents metric.Int64Counter
	// slackLatency is how long acknowledgement took. Slack retries anything it is not
	// answered inside three seconds, so a deployment drifting towards that ceiling is a
	// retry storm it is asking for — and no counter of outcomes would show it coming.
	slackLatency metric.Float64Histogram
}

// The attribute keys, declared once so a typo is a compile error rather than a second time series
// nobody notices is a duplicate.
const (
	dispositionKey = "disposition"
	groupingKey    = "grouping"
)

// newInstruments builds the counters.
//
// A failure to construct one is logged and leaves that counter nil, and every emit below tolerates
// a nil. Telemetry that refuses to start would take intake with it, which trades an observability
// gap for the loss of a customer's alerts — and this is the surface where losing the record is
// worse than anything it was recording.
func newInstruments(logger *slog.Logger) instruments {
	meter := otel.Meter(meterName)
	var built instruments

	var err error
	if built.deliveries, err = meter.Int64Counter("oc.intake.deliveries",
		metric.WithDescription(
			"Webhook deliveries reaching intake, by what was done with them."),
		metric.WithUnit("{delivery}")); err != nil {
		logger.Warn("intake delivery metric unavailable", slog.String("error", err.Error()))
	}
	if built.signals, err = meter.Int64Counter("oc.intake.signals",
		metric.WithDescription("Signals recorded from accepted deliveries."),
		metric.WithUnit("{signal}")); err != nil {
		logger.Warn("intake signal metric unavailable", slog.String("error", err.Error()))
	}
	if built.episodes, err = meter.Int64Counter("oc.intake.episode_groupings",
		metric.WithDescription(
			"Grouping decisions: a new Signal opened an operational episode, or joined one."),
		metric.WithUnit("{signal}")); err != nil {
		logger.Warn("intake grouping metric unavailable", slog.String("error", err.Error()))
	}
	if built.slackEvents, err = meter.Int64Counter("oc.intake.slack_events",
		metric.WithDescription(
			"Slack events reaching intake, by what was done with them."),
		metric.WithUnit("{event}")); err != nil {
		logger.Warn("intake slack metric unavailable", slog.String("error", err.Error()))
	}
	if built.slackLatency, err = meter.Float64Histogram("oc.intake.slack_acknowledgement",
		metric.WithDescription(
			"How long acknowledging one Slack event took, against the vendor's own timeout."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("intake slack latency metric unavailable",
			slog.String("error", err.Error()))
	}
	return built
}

// observeSlackLatency records how long acknowledgement took.
func (i instruments) observeSlackLatency(ctx context.Context, took time.Duration) {
	if i.slackLatency == nil {
		return
	}
	i.slackLatency.Record(ctx, took.Seconds())
}

// countSlackEvent records what happened to one inbound Slack event. The disposition is one
// of this build's own words, for the reason countDelivery's is.
func (i instruments) countSlackEvent(ctx context.Context, disposition string) {
	if i.slackEvents == nil {
		return
	}
	i.slackEvents.Add(ctx, 1,
		metric.WithAttributes(attribute.String(dispositionKey, disposition)))
}

// countDelivery records what happened to one delivery.
//
// The disposition is one of this build's own words. It is never a message, a header or anything a
// caller sent: an attacker who could put a value here could mint an unbounded number of time
// series, which is a denial of service against the monitoring rather than against the product.
func (i instruments) countDelivery(ctx context.Context, disposition string) {
	if i.deliveries == nil {
		return
	}
	i.deliveries.Add(ctx, 1,
		metric.WithAttributes(attribute.String(dispositionKey, disposition)))
}

// countSignals records what an accepted delivery carried and how it grouped.
func (i instruments) countSignals(ctx context.Context, recorded, opened, joined int) {
	if i.signals != nil && recorded > 0 {
		i.signals.Add(ctx, int64(recorded))
	}
	if i.episodes == nil {
		return
	}
	// Reported apart rather than as a ratio. A ratio computed here would be a number nobody could
	// re-aggregate over a window, and the window is the operator's to choose.
	if opened > 0 {
		i.episodes.Add(ctx, int64(opened),
			metric.WithAttributes(attribute.String(groupingKey, "opened")))
	}
	if joined > 0 {
		i.episodes.Add(ctx, int64(joined),
			metric.WithAttributes(attribute.String(groupingKey, "joined")))
	}
}

// The dispositions this surface counts. They are the same words the delivery history uses where
// the two overlap, so an operator moving between a Connection's history and the fleet-wide metric
// is reading one vocabulary.
const (
	dispositionAccepted        = "accepted"
	dispositionDuplicate       = "duplicate"
	dispositionUnauthenticated = "unauthenticated"
	dispositionOversized       = "oversized"
	dispositionIncomplete      = "incomplete"
	dispositionMalformed       = "malformed"
	dispositionRateLimited     = "rate_limited"
	// dispositionUnavailable is OURS rather than the caller's: a database that could not be
	// reached, a Connection naming an Integration this build cannot parse. It is counted
	// separately because it is the one disposition that pages somebody here rather than somebody
	// at the far end.
	dispositionUnavailable = "unavailable"
)

// The dispositions an inbound Slack event is counted under. They are apart from the delivery
// words above because they answer different questions, and each of these is a distinct
// operational story rather than a shade of "refused".
const (
	slackAccepted  = "accepted"
	slackDuplicate = "duplicate"
	// slackRefused is a request that failed authenticity: no signature, a wrong one, a body
	// mutated after signing, or a stale timestamp. Rising means somebody is probing, or the
	// app's signing secret was rotated and this deployment was not told.
	slackRefused = "refused"
	// slackUnknownWorkspace is a correctly signed request naming a workspace this
	// deployment has no installation for. It is answered exactly as a refusal, and counted
	// apart because it means something entirely different: a workspace that uninstalled, or
	// an app registration pointed at the wrong deployment.
	slackUnknownWorkspace = "unknown_workspace"
	slackMalformed        = "malformed"
	slackChallenge        = "challenge"
	// slackNotAddressed is an event this build deliberately ignores: a channel join, an
	// edit, another app posting, or OpenCluster's own message.
	slackNotAddressed = "not_addressed"
	// slackOutsideRollout is an organization the staged rollout has not reached, and
	// slackDisabled is an integration an operator turned off. Both are acknowledged and
	// dropped, and they are counted apart so "we are not answering" can be told from "we
	// are failing to answer".
	slackOutsideRollout = "outside_rollout"
	slackDisabled       = "disabled"
	slackUnavailable    = "unavailable"
	// slackRateLimited is a workspace shedding load, and slackQueued is a message
	// persisted behind an organization's own ceiling on unclaimed turns. Neither is a
	// failure, and both are counted so that "we are deliberately slowing down" can be told
	// from "we are broken".
	slackRateLimited = "rate_limited"
	slackQueued      = "queued"
)

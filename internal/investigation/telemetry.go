package investigation

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/open-cluster/oc-control-plane/internal/investigation"

type Telemetry struct {
	logger *slog.Logger

	queueWait     metric.Float64Histogram
	firstProgress metric.Float64Histogram
	duration      metric.Float64Histogram
	toolDuration  metric.Float64Histogram
	recovered     metric.Int64Counter
}

func (t *Telemetry) Ended(duration time.Duration, status, stoppedBy string) {
	t.ended(duration, status, stoppedBy)
}

func (t *Telemetry) RanTool(run ToolRun) { t.ranTool(run) }

func NewTelemetry(logger *slog.Logger) *Telemetry {
	meter := otel.Meter(meterName)
	built := &Telemetry{logger: logger}

	var err error
	if built.queueWait, err = meter.Float64Histogram("oc.investigation.queue_wait",
		metric.WithDescription("Seconds between an investigation being opened and a "+
			"worker claiming it."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("investigation queue metric unavailable",
			slog.String("error", err.Error()))
	}
	if built.firstProgress, err = meter.Float64Histogram(
		"oc.investigation.time_to_first_progress",
		metric.WithDescription("Seconds from claim to the first event a reader sees."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("investigation progress metric unavailable",
			slog.String("error", err.Error()))
	}
	if built.duration, err = meter.Float64Histogram("oc.investigation.duration",
		metric.WithDescription("Wall-clock seconds of one whole investigation, by how "+
			"it ended."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("investigation duration metric unavailable",
			slog.String("error", err.Error()))
	}
	if built.toolDuration, err = meter.Float64Histogram("oc.investigation.tool_duration",
		metric.WithDescription("Wall-clock seconds of one tool run, by outcome."),
		metric.WithUnit("s")); err != nil {
		logger.Warn("investigation tool metric unavailable",
			slog.String("error", err.Error()))
	}
	if built.recovered, err = meter.Int64Counter("oc.investigation.recovered",
		metric.WithDescription("Investigations failed because their worker's lease "+
			"lapsed."),
		metric.WithUnit("{investigation}")); err != nil {
		logger.Warn("recovery metric unavailable", slog.String("error", err.Error()))
	}
	return built
}

func (t *Telemetry) claimed(openedAt time.Time) {
	if t == nil || t.queueWait == nil || openedAt.IsZero() {
		return
	}
	t.queueWait.Record(context.Background(), time.Since(openedAt).Seconds())
}

func (t *Telemetry) firstEvent(since time.Duration) {
	if t == nil || t.firstProgress == nil {
		return
	}
	t.firstProgress.Record(context.Background(), since.Seconds())
}

func (t *Telemetry) ended(since time.Duration, outcome, stoppedBy string) {
	if t == nil || t.duration == nil {
		return
	}
	attributes := []attribute.KeyValue{attribute.String("outcome", outcome)}
	if stoppedBy != "" {
		attributes = append(attributes, attribute.String("stopped_by", stoppedBy))
	}
	t.duration.Record(context.Background(), since.Seconds(),
		metric.WithAttributes(attributes...))
}

func (t *Telemetry) ranTool(run ToolRun) {
	if t == nil || t.toolDuration == nil {
		return
	}
	t.toolDuration.Record(context.Background(),
		run.FinishedAt.Sub(run.StartedAt).Seconds(),
		metric.WithAttributes(
			attribute.String("tool", run.Tool),
			attribute.String("outcome", outcomeWord(run.Outcome))))
}

func (t *Telemetry) RecoveredStale(count int) {
	if t == nil || t.recovered == nil || count <= 0 {
		return
	}
	t.recovered.Add(context.Background(), int64(count))
}

package investigation

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestToolMetricsKeepDurationOutcomeAndBoundUnknownNames(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	measured := NewTelemetry(slog.New(slog.DiscardHandler))
	started := time.Now()
	for _, run := range []ToolRun{
		{Tool: "github_read_commits", IntegrationID: uuid.New(), Outcome: RunSucceeded},
		{Tool: "github_read_commits", IntegrationID: uuid.New(), Outcome: RunFailed},
		{Tool: "unoffered_one", Outcome: RunFailed},
		{Tool: "unverified_two", Outcome: RunFailed},
	} {
		run.StartedAt, run.FinishedAt = started, started.Add(2*time.Second)
		measured.RanTool(run)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "oc.investigation.tool_duration" {
				continue
			}
			histogram := metric.Data.(metricdata.Histogram[float64])
			if len(histogram.DataPoints) != 3 {
				t.Fatalf("tool metric series = %d, want three bounded name/outcome pairs", len(histogram.DataPoints))
			}
			for _, point := range histogram.DataPoints {
				tool, _ := point.Attributes.Value("tool")
				outcome, _ := point.Attributes.Value("outcome")
				count := uint64(1)
				if tool.AsString() == "unknown" {
					count = 2
				} else if tool.AsString() != "github_read_commits" {
					t.Fatalf("unverified name escaped into metric: %s", tool.AsString())
				}
				if point.Attributes.Len() != 2 || point.Count != count || point.Sum != float64(2*count) ||
					(outcome.AsString() != "succeeded" && outcome.AsString() != "failed") {
					t.Fatalf("unexpected Tool duration/outcome: %+v", point)
				}
			}
			return
		}
	}
	t.Fatal("Tool duration metric is missing")
}

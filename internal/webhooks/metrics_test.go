package webhooks

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestWebhookMetricsExposeOnlyRequestAlertEventAndSlackAcknowledgementSignals(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(previous)

	instruments := newInstruments(slog.New(slog.DiscardHandler))
	instruments.countRequest(context.Background(), surfaceAlert, resultRateLimited)
	instruments.countRequest(context.Background(), surfaceSlack, resultAccepted)
	instruments.countAlertEvents(context.Background(), 2)
	instruments.observeSlackAcknowledgement(context.Background(), 125*time.Millisecond)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, measured := range scope.Metrics {
			found[measured.Name] = true
			switch measured.Name {
			case "oc.webhooks.requests":
				sum, ok := measured.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("request metric data = %T", measured.Data)
				}
				for _, point := range sum.DataPoints {
					if point.Attributes.Len() != 2 {
						t.Fatalf("request metric attributes = %v, want surface and result", point.Attributes)
					}
					if _, ok := point.Attributes.Value("surface"); !ok {
						t.Fatalf("request metric omitted surface: %v", point.Attributes)
					}
					if _, ok := point.Attributes.Value("result"); !ok {
						t.Fatalf("request metric omitted result: %v", point.Attributes)
					}
				}
			case "oc.webhooks.alert_events":
				sum, ok := measured.Data.(metricdata.Sum[int64])
				if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 ||
					sum.DataPoints[0].Attributes.Len() != 0 {
					t.Fatalf("alert event metric = %#v", measured.Data)
				}
			case "oc.webhooks.slack_ack_duration":
				histogram, ok := measured.Data.(metricdata.Histogram[float64])
				if !ok || len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 ||
					histogram.DataPoints[0].Attributes.Len() != 0 {
					t.Fatalf("Slack acknowledgement metric = %#v", measured.Data)
				}
			}
		}
	}
	for _, name := range []string{
		"oc.webhooks.requests",
		"oc.webhooks.alert_events",
		"oc.webhooks.slack_ack_duration",
	} {
		if !found[name] {
			t.Errorf("metric %q was not emitted", name)
		}
	}
	for name := range found {
		if name != "oc.webhooks.requests" && name != "oc.webhooks.alert_events" &&
			name != "oc.webhooks.slack_ack_duration" {
			t.Errorf("obsolete webhook metric %q was emitted", name)
		}
	}
}

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

func TestAlertAdmissionMetricsDistinguishCapacityAndPermanentRefusals(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(previous)

	instruments := newInstruments(slog.New(slog.DiscardHandler))
	instruments.countDelivery(context.Background(), dispositionCapacityRefused)
	instruments.countDelivery(context.Background(), dispositionBatchTooLarge)
	instruments.observeAlertAcceptance(context.Background(), 125*time.Millisecond)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	dispositions := map[string]bool{}
	latencyObservations := uint64(0)
	for _, scope := range collected.ScopeMetrics {
		for _, measured := range scope.Metrics {
			switch measured.Name {
			case "oc.intake.deliveries":
				sum, ok := measured.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("delivery metric data = %T", measured.Data)
				}
				for _, point := range sum.DataPoints {
					if point.Attributes.Len() != 1 {
						t.Fatalf("delivery metric attributes = %v, want only disposition", point.Attributes)
					}
					if value, ok := point.Attributes.Value(dispositionKey); ok {
						dispositions[value.AsString()] = true
					}
				}
			case "oc.intake.alert_acceptance":
				histogram, ok := measured.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("alert acceptance metric data = %T", measured.Data)
				}
				for _, point := range histogram.DataPoints {
					latencyObservations += point.Count
				}
			}
		}
	}
	for _, disposition := range []string{dispositionCapacityRefused, dispositionBatchTooLarge} {
		if !dispositions[disposition] {
			t.Errorf("delivery disposition %q was not emitted", disposition)
		}
	}
	if latencyObservations != 1 {
		t.Fatalf("alert acceptance latency observations = %d, want 1", latencyObservations)
	}
}

package agent

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestModelCallTelemetryKeepsOperationsSignalsWithoutBillingState(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousMeter := otel.GetMeterProvider()
	otel.SetMeterProvider(meterProvider)
	defer otel.SetMeterProvider(previousMeter)

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer otel.SetTracerProvider(previousTracer)

	model := &scriptedModel{next: func(_ int, _ Prompt) (Completion, error) {
		return Completion{
			Model: "model-response", RequestID: "request-1", Stop: StopComplete,
			Usage: TokenUsage{
				Input: Counted(10), Output: Counted(4), CacheWrite: Counted(3),
				CacheRead: Counted(2), Reasoning: Counted(1),
			},
		}, nil
	}}
	telemetry := NewTelemetry(slog.New(slog.DiscardHandler))
	if _, err := telemetry.complete(context.Background(), model,
		Deployment{Provider: "anthropic", Model: "configured-model"}, Prompt{}); err != nil {
		t.Fatal(err)
	}

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	kinds := map[string]bool{}
	for _, scope := range metrics.ScopeMetrics {
		for _, measured := range scope.Metrics {
			found[measured.Name] = true
			if measured.Name != "oc.reasoning.tokens" {
				continue
			}
			sum, ok := measured.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("token metric data = %T", measured.Data)
			}
			for _, point := range sum.DataPoints {
				if value, ok := point.Attributes.Value("kind"); ok {
					kinds[value.AsString()] = true
				}
			}
		}
	}
	for _, name := range []string{"oc.reasoning.calls", "oc.reasoning.tokens", "oc.reasoning.call_duration"} {
		if !found[name] {
			t.Errorf("metric %q was not emitted", name)
		}
	}
	if found["oc.reasoning.spend"] {
		t.Error("monetary spend metric was emitted")
	}
	for _, kind := range []string{"input", "output", "cache_write", "cache_read", "reasoning"} {
		if !kinds[kind] {
			t.Errorf("token kind %q was not emitted", kind)
		}
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d", len(spans))
	}
	attributes := map[string]bool{}
	for _, attribute := range spans[0].Attributes() {
		attributes[string(attribute.Key)] = true
	}
	for _, key := range []string{"oc.reasoning.stop", "oc.reasoning.provider", "oc.reasoning.model"} {
		if !attributes[key] {
			t.Errorf("span attribute %q is missing", key)
		}
	}
	for _, key := range []string{"oc.reasoning.spend_microcents", "oc.reasoning.agent_revision"} {
		if attributes[key] {
			t.Errorf("retired span attribute %q remains", key)
		}
	}
}

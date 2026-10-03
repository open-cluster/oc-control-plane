package integrations

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const connectMeterName = "github.com/open-cluster/oc-control-plane/internal/integrations"

var connects = sync.OnceValue(func() metric.Int64Counter {
	counter, err := otel.Meter(connectMeterName).Int64Counter("oc.integrations.connects",
		metric.WithDescription(
			"Provider installation flows that came back, by what the return established."),
		metric.WithUnit("{connect}"))
	if err != nil {
		return nil
	}
	return counter
})

func countConnect(ctx context.Context, typeKey string, outcome connectOutcome) {
	counter := connects()
	if counter == nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("integration_type", typeKey),
		attribute.String("outcome", string(outcome))))
}

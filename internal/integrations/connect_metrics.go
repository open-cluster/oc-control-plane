package integrations

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// A customer who presses Connect and does not come back connected produces nothing anybody
// here would notice: the failure happens in their browser, on somebody else's site, and the
// only trace is a log line for a request that may never have been made. This is the counter
// that makes an onboarding flow that has stopped working visible before a support ticket does.
const connectMeterName = "github.com/open-cluster/oc-control-plane/internal/integrations"

// connects is built once. An instrument rebuilt per request is a new time series per request,
// and this is reached by a browser.
var connects = sync.OnceValue(func() metric.Int64Counter {
	counter, err := otel.Meter(connectMeterName).Int64Counter("oc.integrations.connects",
		metric.WithDescription(
			"Provider installation flows that came back, by what the return established."),
		metric.WithUnit("{connect}"))
	if err != nil {
		// A counter that could not be built is nil, and countConnect tolerates a nil.
		// Telemetry that refused to start would take the installation flow with it.
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

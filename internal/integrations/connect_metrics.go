package integrations

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// WHAT AN OPERATOR WATCHING THE INSTALLATION FLOW CAN SEE.
//
// A customer who presses Connect and does not come back connected produces nothing anybody
// here would notice: the failure happens in their browser, on somebody else's site, and the
// only trace is a log line for a request that may never have been made. This is the counter
// that makes an onboarding flow that has stopped working visible before a support ticket does.
//
// NO ORGANIZATION LABEL, and no vendor message. What is attributed is the closed set of
// outcome words this package already owns — a value a caller or a provider could choose would
// mint an unbounded number of time series, which is a denial of service against the monitoring
// rather than against the product.

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

// countConnect records one completed return trip.
func countConnect(ctx context.Context, typeKey string, outcome connectOutcome) {
	counter := connects()
	if counter == nil {
		return
	}
	// The type KEY is this build's own vocabulary and a closed set — the catalog is
	// compiled — so it is safe to attribute by, and it is what tells "GitHub's flow is
	// broken" from "everything is broken".
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("integration_type", typeKey),
		attribute.String("outcome", string(outcome))))
}

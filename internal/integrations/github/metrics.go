package github

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// WHAT AN OPERATOR WATCHING GITHUB CAN SEE.
//
// A misregistered App fails every connect attempt in this deployment and looks, from any
// one tenant's screen, like that tenant's mistake. This counter is what makes the shape of
// it visible: verifications refused for a credential the App itself could not present are
// a rising number, and the reason names which half is wrong.
//
// NO ORGANIZATION LABEL and NO ACCOUNT LABEL. Tenant identity belongs on a span; a label
// taken from a customer's GitHub account would be an unbounded set of time series chosen
// by somebody outside this deployment.

const meterName = "github.com/open-cluster/oc-control-plane/internal/integrations/github"

// reasonKey attributes a verification with the closed word this build judged it by.
const reasonKey = "reason"

// verifications is built once for the process. A failure to construct it leaves the
// counter nil and every emit tolerates that: telemetry that refused to start would take
// connecting GitHub down with it, which trades an observability gap for the capability
// being observed.
var verifications = sync.OnceValue(func() metric.Int64Counter {
	counter, err := otel.Meter(meterName).Int64Counter(
		"oc.github.installation_verifications",
		metric.WithDescription(
			"GitHub App installation verifications, by what this deployment judged them."),
		metric.WithUnit("{verification}"))
	if err != nil {
		return nil
	}
	return counter
})

func countVerification(ctx context.Context, outcome judged) {
	counter := verifications()
	if counter == nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(reasonKey, outcome.reason),
		attribute.String("status", outcome.Status.String())))
}

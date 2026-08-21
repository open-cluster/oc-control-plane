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

// The attribute keys, declared once so a typo is a compile error rather than a second time
// series nobody notices is a duplicate. Both values are closed sets this build owns; a
// value a customer's GitHub account could choose would be an unbounded number of series.
const (
	reasonKey  = "reason"
	outcomeKey = "outcome"
)

// verifications is built once for the process. A failure to construct it leaves the
// counter nil and every emit tolerates that: telemetry that refused to start would take
// connecting GitHub down with it, which trades an observability gap for the capability
// being observed.
var checks = sync.OnceValue(func() metric.Int64Counter {
	counter, err := otel.Meter(meterName).Int64Counter(
		"oc.github.installation_checks",
		metric.WithDescription(
			"Checks of a GitHub App installation — proving one at connect time and "+
				"verifying one afterwards — by what this deployment judged them."),
		metric.WithUnit("{check}"))
	if err != nil {
		return nil
	}
	return counter
})

// countVerification records one verification probe.
func countVerification(ctx context.Context, outcome judged) {
	countCheck(ctx, outcome.reason, outcome.Status.String())
}

// countCheck records one check of an installation. Proving an association at connect time
// is counted here too: a deployment whose OAuth client is misregistered fails every connect
// and verifies nothing, so counting only the probe would leave the commonest broken App
// configuration invisible — which is the case this counter exists for.
func countCheck(ctx context.Context, reason, outcome string) {
	counter := checks()
	if counter == nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(reasonKey, reason),
		attribute.String(outcomeKey, outcome)))
}

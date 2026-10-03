package github

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/open-cluster/oc-control-plane/internal/integrations/github"

const (
	reasonKey  = "reason"
	outcomeKey = "outcome"
)

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

func countVerification(ctx context.Context, outcome judged) {
	countCheck(ctx, outcome.reason, outcome.Status.String())
}

func countCheck(ctx context.Context, reason, outcome string) {
	counter := checks()
	if counter == nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(reasonKey, reason),
		attribute.String(outcomeKey, outcome)))
}

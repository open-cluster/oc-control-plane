package prometheus

import (
	"context"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	prom "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const queryGrant = "prometheus:query"

func Definition() integrations.Definition {
	return integrations.Definition{
		Key:         "prometheus",
		Name:        "Prometheus",
		Category:    integrations.CategoryObservability,
		Description: "Discover metric names and query Prometheus metrics to compare service errors, latency and resource usage.",
		Config: []integrations.Field{
			{
				Key:      "url",
				Type:     integrations.FieldString,
				Required: true,
			},
			{
				Key:    "bearerToken",
				Type:   integrations.FieldString,
				Secret: true,
			},
		},
		Probe: probe,
		Tools: []integrations.Tool{
			discoverMetricsTool(),
			queryTool(),
		},
	}
}

func probe(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
	address, _ := input.Integration.Configuration["url"].(string)
	client, err := newClient(address, input.Credential)
	if err != nil {
		return integrations.Verification{Status: integrations.StatusFailed, Note: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	value, _, _, err := client.Query(ctx, "vector(1)", time.Now().UTC(), prom.WithTimeout(queryTimeout), prom.WithLimit(1))
	if err != nil {
		return integrations.Verification{Status: integrations.StatusFailed, Note: queryError(err).Error()}
	}
	vector, ok := value.(model.Vector)
	if !ok || len(vector) != 1 || vector[0] == nil || vector[0].Histogram != nil || vector[0].Value != 1 {
		return integrations.Verification{Status: integrations.StatusFailed, Note: "Prometheus did not return the expected verification query result"}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified, Note: "Prometheus query access verified", Grants: []string{queryGrant},
	}
}

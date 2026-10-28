package tempo

import (
	"context"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const queryGrant = "tempo:query"

func Definition() integrations.Definition {
	return integrations.Definition{
		Key:         "tempo",
		Name:        "Tempo",
		Category:    integrations.CategoryObservability,
		Description: "Search traces and read bounded span relationships from a Tempo source.",
		Config: []integrations.Field{
			{Key: "url", Type: integrations.FieldString, Required: true},
			{Key: "token", Type: integrations.FieldString, Secret: true},
			{Key: "auth", Type: integrations.FieldString, Required: true, Options: []string{"bearer", "basic"}},
			{Key: "username", Type: integrations.FieldString},
			{Key: "tenantId", Type: integrations.FieldString},
		},
		Probe: probe,
		Tools: []integrations.Tool{
			searchTracesTool(),
			getTraceTool(),
		},
	}
}

func probe(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
	client, err := newClient(input.Integration.Configuration, input.Credential)
	if err == nil {
		until := time.Now().UTC().Truncate(time.Second)
		var response searchResponse
		response, err = client.search(ctx, "{}", until.Add(-time.Minute), until, 1)
		if err == nil {
			_, _, err = searchSummaries(response.Traces[:min(1, len(response.Traces))])
		}
	}
	if err != nil {
		return integrations.Verification{Status: integrations.StatusFailed, Note: err.Error()}
	}
	return integrations.Verification{Status: integrations.StatusVerified, Note: "Tempo trace search access verified", Grants: []string{queryGrant}}
}

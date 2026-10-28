package loki

import (
	"context"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const queryGrant = "loki:query"

func Definition() integrations.Definition {
	return integrations.Definition{
		Key:         "loki",
		Name:        "Loki",
		Category:    integrations.CategoryObservability,
		Description: "Query bounded log excerpts from an approved Loki source during an Investigation.",
		Config: []integrations.Field{
			{Key: "username", Type: integrations.FieldString},
			{Key: "tenantId", Type: integrations.FieldString},
			{Key: "url", Type: integrations.FieldString, Required: true},
			{Key: "token", Type: integrations.FieldString, Secret: true},
			{Key: "auth", Type: integrations.FieldString, Required: true, Options: []string{"bearer", "basic"}},
		},
		Probe: probe,
		Tools: []integrations.Tool{queryTool()},
	}
}

func probe(ctx context.Context, input integrations.ProbeInput) integrations.Verification {
	client, err := newClient(input.Integration.Configuration, input.Credential)
	if err == nil {
		until := time.Now().UTC()
		_, err = client.query(ctx, `{job=~".+"}`, until.Add(-time.Minute), until, 1, "backward")
	}
	if err != nil {
		return integrations.Verification{Status: integrations.StatusFailed, Note: err.Error()}
	}
	return integrations.Verification{
		Status: integrations.StatusVerified,
		Note:   "Loki log query access verified",
		Grants: []string{queryGrant},
	}
}

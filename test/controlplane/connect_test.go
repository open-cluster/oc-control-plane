package controlplane

import (
	"net/http"
	"net/url"
	"testing"
)

func stateOf(t *testing.T, started string) string {
	t.Helper()
	var answer struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}
	decodeInto(t, started, &answer)
	parsed, err := url.Parse(answer.AuthorizationURL)
	if err != nil {
		t.Fatalf("the authorization url %q is not a url: %v", answer.AuthorizationURL, err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatalf("the authorization url carries no state: %s", answer.AuthorizationURL)
	}
	return state
}

type landedBody struct {
	Connect       string `json:"connect"`
	IntegrationID string `json:"integrationId"`
	Note          string `json:"note"`
}

func (p *integrationPlane) integrations(t *testing.T, organization string) []integrationBody {
	t.Helper()
	status, body := p.call(t, http.MethodGet, p.base(organization)+"/integrations", nil)
	if status != http.StatusOK {
		t.Fatalf("listing integrations = %d: %s", status, body)
	}
	var listed struct {
		Items []integrationBody `json:"items"`
	}
	decodeInto(t, body, &listed)
	return listed.Items
}

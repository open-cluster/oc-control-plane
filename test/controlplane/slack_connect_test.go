package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func startSlackInstallPlane(t *testing.T, vendor *vendorFake, console string) *integrationPlane {
	t.Helper()

	apiAddress := freeAddress(t)
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		cfg.SlackClientID = "4444.5555"
		cfg.SlackClientSecret = "the-slack-client-secret"
		cfg.SlackSigningSecret = "the-slack-signing-secret"
		cfg.PublicURL = "http://" + apiAddress
		cfg.PublicURL = console
		dsn = cfg.DatabaseDSN
	}, app.Options{SlackAPIURL: vendor.URL})
	return &integrationPlane{controlPlane: plane, api: apiAddress, dsn: dsn}
}

func (p *integrationPlane) pressConnectSlack(t *testing.T, organization string) (int, string) {
	t.Helper()
	return p.call(t, http.MethodPost,
		p.base(organization)+"/integration-types/slack/connect?returnTo=/integrations", nil)
}

func (p *integrationPlane) returnFromSlack(t *testing.T, parameters url.Values) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet,
		"http://"+p.api+"/api/v1/integrations/connect/callback?"+
			parameters.Encode(), nil)
	if err != nil {
		t.Fatalf("building callback request: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: p.sessionCookie})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("returning from Slack: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw)
	}
	landed, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parsing callback landing: %v", err)
	}
	outcome := landed.Query().Get("connect")
	status := http.StatusBadRequest
	if outcome == "connected" {
		status = http.StatusOK
	}
	body, err := json.Marshal(map[string]string{
		"connect": outcome, "integrationId": landed.Query().Get("integration"),
	})
	if err != nil {
		t.Fatalf("encoding callback outcome: %v", err)
	}
	return status, string(body)
}

func connectSlack(t *testing.T, plane *integrationPlane, code string) (int, string) {
	t.Helper()

	status, started := plane.pressConnectSlack(t, surfaceOrg)
	if status != http.StatusOK {
		t.Fatalf("pressing connect = %d: %s", status, started)
	}
	return plane.returnFromSlack(t, url.Values{
		"state": {stateOf(t, started)}, "code": {code},
	})
}

func TestConnectingSlackSealsTheBotTokenAndRecordsTheWorkspace(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackInstallPlane(t, vendor, "")
	status, detailBody := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/integration-types/slack", nil)
	var integrationType struct {
		SupportsConnect bool `json:"supportsConnect"`
	}
	decodeInto(t, detailBody, &integrationType)
	if status != http.StatusOK || !integrationType.SupportsConnect {
		t.Fatalf("configured Slack type detail = %d: %s", status, detailBody)
	}

	status, started := plane.pressConnectSlack(t, surfaceOrg)
	if status != http.StatusOK {
		t.Fatalf("starting Slack authorization = %d: %s", status, started)
	}
	var authorization struct {
		URL string `json:"authorizationUrl"`
	}
	decodeInto(t, started, &authorization)
	authorizationURL, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	scopes := "," + authorizationURL.Query().Get("scope") + ","
	if strings.Contains(scopes, ",assistant:write,") || !strings.Contains(scopes, ",chat:write,") || !strings.Contains(scopes, ",app_mentions:read,") {
		t.Fatalf("Slack authorization requests incorrect scopes: %s", scopes)
	}
	status, landed := plane.returnFromSlack(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
	})
	if status != http.StatusOK {
		t.Fatalf("a valid slack callback = %d: %s", status, landed)
	}
	var outcome landedBody
	decodeInto(t, landed, &outcome)
	if outcome.Connect != "connected" {
		t.Fatalf("outcome = %q, want connected: %s", outcome.Connect, outcome.Note)
	}

	if codes := vendor.codesExchanged(); len(codes) != 1 || codes[0] != "the-authorization-code" {
		t.Errorf("codes exchanged = %v, want the one code exactly once", codes)
	}
	if vendor.exchangeSecret != "the-slack-client-secret" {
		t.Errorf("the exchange presented %q as the client secret", vendor.exchangeSecret)
	}

	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("the tenant holds %d integrations, want one", len(listed))
	}
	installed := listed[0]
	if installed.Status != "verified" {
		t.Errorf("status = %q, want verified", installed.Status)
	}

	if len(installed.Configuration) != 0 {
		t.Errorf("discovered workspace identity leaked into configuration: %+v",
			installed)
	}

	if installed.Credential == nil || !installed.Credential.Configured {
		t.Fatal("no credential is recorded; the integration cannot read anything")
	}
	if strings.Contains(landed, "xoxb-installed-token") {
		t.Fatal("the bot token is readable from the callback answer")
	}
	status, read := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integrations/"+installed.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the integration back = %d: %s", status, read)
	}
	if strings.Contains(read, "xoxb-installed-token") {
		t.Fatal("the bot token is readable from the integration surface")
	}
}

func TestTheSlackCallbackIgnoresAnOrganizationInItsQuery(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackInstallPlane(t, vendor, "")

	status, started := plane.pressConnectSlack(t, surfaceOrg)
	if status != http.StatusOK {
		t.Fatalf("pressing connect = %d: %s", status, started)
	}
	status, landed := plane.returnFromSlack(t, url.Values{
		"state":        {stateOf(t, started)},
		"code":         {"the-authorization-code"},
		"organization": {neighbourOrg},
		"org_id":       {neighbourOrg},
	})
	if status != http.StatusOK {
		t.Fatalf("the callback = %d: %s", status, landed)
	}

	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("the starting tenant holds %d integrations, want one", len(listed))
	}
	if len(listed[0].Configuration) != 0 {
		t.Errorf("callback changed editable configuration: %+v", listed[0])
	}
	defer plane.switchOrganization(t, neighbourOrg)()
	status, body := plane.call(t, http.MethodGet, plane.base(neighbourOrg)+"/integrations", nil)
	var neighbor struct {
		Items []json.RawMessage `json:"items"`
	}
	decodeInto(t, body, &neighbor)
	if status != http.StatusOK || len(neighbor.Items) != 0 {
		t.Errorf("the neighbour's listing = %d, want empty list: %s", status, body)
	}
}

func TestConnectingTheSameWorkspaceAgainDoesNotDuplicateIt(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackInstallPlane(t, vendor, "")

	if status, landed := connectSlack(t, plane, "the-authorization-code"); status != http.StatusOK {
		t.Fatalf("the first connect = %d: %s", status, landed)
	}
	if status, landed := connectSlack(t, plane, "the-authorization-code"); status != http.StatusOK {
		t.Fatalf("the second connect = %d: %s", status, landed)
	}

	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("connecting one workspace twice left %d integrations, want one", len(listed))
	}
	previousIntegrationID := listed[0].ID
	if status, body := plane.call(t, http.MethodDelete, plane.base(surfaceOrg)+"/integrations/"+previousIntegrationID, nil); status != http.StatusNoContent {
		t.Fatalf("disconnect = %d: %s", status, body)
	}
	if status, _ := connectSlack(t, plane, "a-code-from-somewhere-else"); status == http.StatusOK {
		t.Fatal("reconnect accepted an invalid authorization code")
	}
	if integrations := plane.integrations(t, surfaceOrg); len(integrations) != 0 {
		t.Fatalf("failed authorization restored an integration: %+v", integrations)
	}
	if status, body := connectSlack(t, plane, "the-authorization-code"); status != http.StatusOK {
		t.Fatalf("fresh authorization = %d: %s", status, body)
	}
	if integrations := plane.integrations(t, surfaceOrg); len(integrations) != 1 || integrations[0].ID == previousIntegrationID {
		t.Fatalf("reconnect did not create a new integration: %+v", integrations)
	}
}

func TestAnUnexchangeableSlackCodeBindsNothing(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackInstallPlane(t, vendor, "")

	status, landed := connectSlack(t, plane, "a-code-from-somewhere-else")
	if status == http.StatusOK {
		t.Errorf("an unexchangeable code = %d, want a refusal: %s", status, landed)
	}
	if got := len(plane.integrations(t, surfaceOrg)); got != 0 {
		t.Errorf("a refused exchange left %d integrations behind", got)
	}
}

func TestADeploymentWithNoSlackAppKeepsTheForm(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)

	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/integration-types/slack", nil)
	if status != http.StatusOK {
		t.Fatalf("Slack detail = %d: %s", status, body)
	}
	var integrationType struct {
		SupportsConnect bool `json:"supportsConnect"`
	}
	decodeInto(t, body, &integrationType)
	if integrationType.SupportsConnect {
		t.Fatal("unconfigured Slack advertised Connect")
	}

	status, refused := plane.pressConnectSlack(t, surfaceOrg)
	if status != http.StatusBadRequest {
		t.Errorf("starting a flow this deployment cannot serve = %d, want 400: %s",
			status, refused)
	}
}

func TestAPastedSlackTokenOffersItsGrantedTools(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackInstallPlane(t, vendor, "")

	status, body := plane.createSlack(t, "Pasted Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("pasting a token = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	if reported := created.Integration.tool(t, "slack_list_channels"); !reported.Available {
		t.Errorf("a pasted token lost a read Tool it had: %+v", reported)
	}
}

func TestAConnectedSlackInstallationReportsItsTools(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackInstallPlane(t, vendor, "")

	if status, landed := connectSlack(t, plane, "the-authorization-code"); status != http.StatusOK {
		t.Fatalf("connecting = %d: %s", status, landed)
	}
	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("the tenant holds %d integrations, want one", len(listed))
	}
	if reported := listed[0].tool(t, "slack_list_channels"); !reported.Available {
		t.Errorf("list channels is unavailable on a connected installation: %+v", reported)
	}
}

package main

import (
	"crypto/sha256"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

// ONE-CLICK SLACK INSTALLATION AT THE COMPOSITION SEAM.
//
// These drive the composed process over HTTP against a fake Slack and assert what a
// customer or an attacker would observe: the HTTP answer, what ended up in the database,
// and which Slack endpoints were asked with which credential. The credential never travels
// through the browser, and the tenant never comes from the callback's query.

// startSlackInstallPlane starts a control plane whose Slack app is registered, so the
// integration surface offers Connect Slack rather than the pasted-token form.
func startSlackInstallPlane(t *testing.T, vendor *vendorFake, console string) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.Assignments[neighbourOrg] = "shared"
		cfg.SlackAPIURL = vendor.URL
		cfg.SlackClientID = "4444.5555"
		cfg.SlackClientSecret = "the-slack-client-secret"
		cfg.SlackSigningSecret = "the-slack-signing-secret"
		cfg.OperatorPublicURL = "http://" + operatorAddress
		cfg.OperatorConsoleURL = console
	})
	return &integrationPlane{controlPlane: plane, operator: operatorAddress}
}

// pressConnectSlack presses the button and returns where the browser would be sent.
func (p *integrationPlane) pressConnectSlack(t *testing.T, organization string) (int, string) {
	t.Helper()
	return p.call(t, http.MethodPost,
		p.base(organization)+"/integration-types/slack/connect?returnTo=/integrations", nil)
}

// returnFromSlack is the browser coming back from the authorization screen.
func (p *integrationPlane) returnFromSlack(t *testing.T, parameters url.Values) (int, string) {
	t.Helper()
	return p.call(t, http.MethodGet,
		"http://"+p.operator+"/operator/v1/integrations/connect/callback?"+
			parameters.Encode(), nil)
}

// connectSlack drives the whole flow once and returns the callback's answer.
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

// The flow the whole slice exists for: a customer presses one button and lands on an
// integration that is already verified and active, with no token having passed through
// their clipboard or their browser.
func TestConnectingSlackSealsTheBotTokenAndRecordsTheWorkspace(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackInstallPlane(t, vendor, "")

	status, landed := connectSlack(t, plane, "the-authorization-code")
	if status != http.StatusOK {
		t.Fatalf("a valid slack callback = %d: %s", status, landed)
	}
	var outcome landedBody
	decodeInto(t, landed, &outcome)
	if outcome.Connect != "connected" {
		t.Fatalf("outcome = %q, want connected: %s", outcome.Connect, outcome.Note)
	}

	// The exchange happened server-side, once, with the client secret in the body.
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
	if installed.Status != "active" {
		t.Errorf("status = %q, want active", installed.Status)
	}

	// The workspace is recorded and visible, so an operator can confirm they connected
	// the right one, and so that reconnecting re-verifies rather than duplicating.
	if installed.Configuration["teamId"] != "T0ACME" {
		t.Errorf("configuration = %+v; the workspace was not recorded",
			installed.Configuration)
	}

	// The bot token is sealed and never rendered. A credential that reached a response
	// once has reached a browser history, a proxy log and a screenshot.
	if installed.Credential == nil || installed.Credential.Fingerprint == "" {
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

// The documented attack. An organization identifier arriving in the callback's query must
// bind nothing: the tenant comes from the flow the state redeemed, and the query is not
// read at all.
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

	// The tenant that started the flow has it, and it names the workspace that was
	// actually installed rather than anything the query asked for.
	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("the starting tenant holds %d integrations, want one", len(listed))
	}
	if listed[0].Configuration["teamId"] != "T0ACME" {
		t.Errorf("configuration = %+v; the callback steered what was recorded",
			listed[0].Configuration)
	}
	// The bootstrap credential reaches one organization, so the neighbour is answered 404
	// by the guard — which is also the proof that nothing was written there under a
	// credential that could not have reached it.
	status, body := plane.call(t, http.MethodGet, plane.base(neighbourOrg)+"/integrations", nil)
	if status != http.StatusNotFound {
		t.Errorf("the neighbour's listing = %d, want 404: %s", status, body)
	}
}

// Reconnecting the same workspace re-verifies what exists instead of leaving a customer
// with two integrations for one Slack. This is what recording the workspace buys.
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
}

// A code Slack will not exchange binds nothing and leaves nothing behind.
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

// A deployment that registered no Slack app keeps the pasted-token form. This is the
// air-gapped path, and it stays supported.
func TestADeploymentWithNoSlackAppKeepsTheForm(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)

	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, body)
	}
	var listed struct {
		Types []struct {
			Key             string `json:"key"`
			SupportsConnect bool   `json:"supportsConnect"`
		} `json:"types"`
	}
	decodeInto(t, body, &listed)
	for _, entry := range listed.Types {
		if entry.Key == "slack" && entry.SupportsConnect {
			t.Error("a deployment with no slack app offered a connect button that " +
				"cannot finish")
		}
	}

	status, refused := plane.pressConnectSlack(t, surfaceOrg)
	if status != http.StatusBadRequest {
		t.Errorf("starting a flow this deployment cannot serve = %d, want 400: %s",
			status, refused)
	}
}

// A pasted token is a credential for reading, not an app somebody installed, so nothing
// can route a mention to it. It must say so rather than claiming an agent that will never
// answer.
func TestAPastedSlackTokenDoesNotClaimTheInboundCapabilities(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackInstallPlane(t, vendor, "")

	status, body := plane.createSlack(t, "Pasted Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("pasting a token = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	for _, inbound := range []string{
		"slack.agent_conversations", "slack.mentions", "slack.thread_replies",
	} {
		reported := created.Integration.capability(t, inbound)
		if reported.Available {
			t.Errorf("%s reads as available on a pasted token, which names no "+
				"installation to deliver events to", inbound)
		}
		if !strings.Contains(reported.Reason, "pasted token") {
			t.Errorf("%s says %q, which does not tell the operator to connect Slack",
				inbound, reported.Reason)
		}
	}

	// The reads it CAN do are unaffected. A pasted token keeps working exactly as it did.
	if reported := created.Integration.capability(t, "slack.list_channels"); !reported.Available {
		t.Errorf("a pasted token lost a read capability it had: %+v", reported)
	}
}

// A connected installation claims the inbound capabilities, because there is now an
// installation to route to and a deployment serving events.
func TestAConnectedSlackInstallationClaimsTheInboundCapabilities(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackInstallPlane(t, vendor, "")

	if status, landed := connectSlack(t, plane, "the-authorization-code"); status != http.StatusOK {
		t.Fatalf("connecting = %d: %s", status, landed)
	}
	listed := plane.integrations(t, surfaceOrg)
	if len(listed) != 1 {
		t.Fatalf("the tenant holds %d integrations, want one", len(listed))
	}
	for _, inbound := range []string{
		"slack.agent_conversations", "slack.mentions", "slack.thread_replies",
	} {
		if reported := listed[0].capability(t, inbound); !reported.Available {
			t.Errorf("%s is unavailable on a connected installation: %+v", inbound, reported)
		}
	}
}

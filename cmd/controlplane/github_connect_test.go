package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

// THE ONE-CLICK INSTALLATION AT THE COMPOSITION SEAM.
//
// These drive the composed process over HTTP against a fake GitHub and assert what an
// attacker or a customer would observe: the HTTP answer, what ended up in the database, and
// which GitHub endpoints were asked with which credential.
//
// The test the whole flow exists for is the first one: a callback naming an installation the
// authenticated GitHub user cannot administer binds nothing and creates nothing.

// installFake is a GitHub that speaks the installation flow as well as the App API: the
// OAuth code exchange, the installations one authenticated person may administer, and the
// App-credential reads the immediate verification makes.
type installFake struct {
	*httptest.Server

	mu sync.Mutex
	// reachable is what /user/installations answers: the installations the authenticated
	// person can administer. The callback's installation id is refused unless it is here.
	reachable []int64
	// accounts names the account each installation belongs to; anything unnamed is
	// acme-corp, which is the single-account case most of these tests want.
	accounts map[int64]string
	// exchanged records every authorization code the fake was asked to exchange.
	exchanged []string
	// credentials records the Authorization header each path was asked with, so a test can
	// assert WHICH credential reached WHICH endpoint.
	credentials map[string]string
	suspended   bool
	// knownCode is the one code the fake will exchange; anything else is refused.
	knownCode string
}

const installedToken = "gho_user_access_token"

func newInstallFake(t *testing.T, reachable ...int64) *installFake {
	t.Helper()

	fake := &installFake{
		reachable:   reachable,
		accounts:    map[int64]string{},
		credentials: map[string]string{},
		knownCode:   "the-authorization-code",
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			fake.mu.Lock()
			fake.credentials[request.URL.Path] = request.Header.Get("Authorization")
			known, suspended := fake.knownCode, fake.suspended
			reachable := append([]int64(nil), fake.reachable...)
			accounts := map[int64]string{}
			for id, login := range fake.accounts {
				accounts[id] = login
			}
			fake.mu.Unlock()

			switch {
			case request.URL.Path == "/login/oauth/access_token":
				_ = request.ParseForm()
				code := request.Form.Get("code")
				fake.mu.Lock()
				fake.exchanged = append(fake.exchanged, code)
				fake.mu.Unlock()
				writer.Header().Set("Content-Type", "application/json")
				if code != known {
					_, _ = writer.Write([]byte(
						`{"error":"bad_verification_code","error_description":"expired"}`))
					return
				}
				_, _ = writer.Write([]byte(
					`{"access_token":"` + installedToken + `","token_type":"bearer"}`))

			case request.URL.Path == "/user/installations":
				writer.Header().Set("Content-Type", "application/json")
				entries := make([]string, 0, len(reachable))
				for _, id := range reachable {
					entries = append(entries, `{"id":`+itoa(id)+
						`,"account":{"login":"`+login(accounts, id)+
						`","type":"Organization"}}`)
				}
				_, _ = writer.Write([]byte(`{"total_count":` + itoa(int64(len(entries))) +
					`,"installations":[` + strings.Join(entries, ",") + `]}`))

			case strings.HasSuffix(request.URL.Path, "/access_tokens"):
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusCreated)
				_, _ = writer.Write([]byte(
					`{"token":"ghs_minted","expires_at":"2036-01-01T00:00:00Z"}`))

			case request.URL.Path == "/installation/repositories":
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
					{"id":1296269,"name":"payments","full_name":"acme-corp/payments"},
					{"id":1296270,"name":"deploy","full_name":"acme-corp/deploy"}]}`))

			case strings.HasPrefix(request.URL.Path, "/app/installations/"):
				writer.Header().Set("Content-Type", "application/json")
				id := strings.TrimPrefix(request.URL.Path, "/app/installations/")
				if !holds(reachable, id) {
					writer.WriteHeader(http.StatusNotFound)
					_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
					return
				}
				suspendedAt := "null"
				if suspended {
					suspendedAt = `"2026-08-01T00:00:00Z"`
				}
				_, _ = writer.Write([]byte(`{"id":` + id + `,
					"account":{"login":"` + loginNamed(accounts, id) +
					`","type":"Organization"},
					"repository_selection":"selected","suspended_at":` + suspendedAt + `}`))

			default:
				t.Errorf("the fake vendor was asked for %q", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *installFake) credentialFor(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentials[path]
}

func (f *installFake) codesExchanged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.exchanged...)
}

func (f *installFake) uninstall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable = nil
}

// login is the account an installation belongs to, defaulting to the one account most of
// these tests use.
func login(accounts map[int64]string, id int64) string {
	if named, ok := accounts[id]; ok {
		return named
	}
	return "acme-corp"
}

// loginNamed is login for a path segment that arrived as text.
func loginNamed(accounts map[int64]string, id string) string {
	for known, named := range accounts {
		if itoa(known) == id {
			return named
		}
	}
	return "acme-corp"
}

func holds(ids []int64, wanted string) bool {
	for _, id := range ids {
		if itoa(id) == wanted {
			return true
		}
	}
	return false
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }

// startInstallPlane starts a control plane whose GitHub provider offers the installation
// flow and reaches the fake for both the API and the browser origin.
func startInstallPlane(t *testing.T, vendor *installFake, console string) *integrationPlane {
	t.Helper()
	return startInstallPlaneAs(t, vendor, console, "")
}

// startInstallPlaneAs is the same plane with the bootstrap credential holding a named role,
// so the authorization on starting a flow can be exercised rather than assumed.
func startInstallPlaneAs(
	t *testing.T, vendor *installFake, console, role string,
) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.Assignments[neighbourOrg] = "shared"
		dsn = cfg.Placements["shared"]
		cfg.GitHubAppID = "12345"
		cfg.GitHubAppKey = appKeyPEM(t)
		cfg.GitHubAPIURL = vendor.URL
		cfg.GitHubWebURL = vendor.URL
		cfg.GitHubAppSlug = "opencluster"
		cfg.GitHubClientID = "Iv1.deployment"
		cfg.GitHubClientSecret = "the-client-secret"
		cfg.OperatorPublicURL = "http://" + operatorAddress
		cfg.OperatorConsoleURL = console
		cfg.OperatorTokenRole = role
	})
	return &integrationPlane{controlPlane: plane, operator: operatorAddress, dsn: dsn}
}

// startConnect presses Connect GitHub and returns the provider URL the browser is sent to.
func (p *integrationPlane) startConnect(t *testing.T, organization string) (int, string) {
	t.Helper()
	return p.call(t, http.MethodPost,
		p.base(organization)+"/integration-types/github/connect?returnTo=/integrations", nil)
}

// stateOf reads the state out of the authorization URL, which is the only thing that
// travels through the browser.
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

// returnFromGitHub is the browser coming back. Extra query parameters are the attacker's
// additions, and the test names them explicitly.
func (p *integrationPlane) returnFromGitHub(
	t *testing.T, parameters url.Values,
) (int, string) {
	t.Helper()
	return p.call(t, http.MethodGet,
		"http://"+p.operator+"/operator/v1/integrations/connect/callback?"+
			parameters.Encode(), nil)
}

type landedBody struct {
	Connect       string `json:"connect"`
	IntegrationID string `json:"integrationId"`
	Note          string `json:"note"`
}

// integrations lists a tenant's, which is where the assertions about what was recorded are
// made: the wire is the seam, not the store.
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

// THE TEST THE SPEC EXISTS FOR. GitHub's own documentation warns that the installation id a
// browser carries back can be spoofed. Here it is spoofed: the flow is started honestly, and
// the callback names an installation the authenticated GitHub account cannot administer.
// Nothing may be bound.
func TestASpoofedInstallationIDInTheCallbackBindsNothing(t *testing.T) {
	// The authenticated person can administer 77 and nothing else. The callback claims 99,
	// which belongs to somebody else entirely.
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	status, started := plane.startConnect(t, surfaceOrg)
	if status != http.StatusOK {
		t.Fatalf("starting a connect = %d: %s", status, started)
	}

	status, landed := plane.returnFromGitHub(t, url.Values{
		"state":           {stateOf(t, started)},
		"code":            {"the-authorization-code"},
		"installation_id": {"99"},
		"setup_action":    {"install"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a spoofed installation id = %d, want 400: %s", status, landed)
	}
	var answer landedBody
	decodeInto(t, landed, &answer)
	if answer.Connect != "unproven" {
		t.Errorf("a spoofed installation landed as %q: %s", answer.Connect, landed)
	}
	if answer.IntegrationID != "" {
		t.Errorf("a spoofed installation produced integration %s", answer.IntegrationID)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Fatalf("a spoofed installation left %d integrations behind: %+v", len(found), found)
	}
}

// The happy path: the customer presses one button, chooses their account and repositories on
// GitHub, and lands back with the integration already active and the account recorded.
func TestConnectingGitHubBindsProbesAndRecordsWhatAnswered(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	status, landed := plane.returnFromGitHub(t, url.Values{
		"state":           {stateOf(t, started)},
		"code":            {"the-authorization-code"},
		"installation_id": {"77"},
		"setup_action":    {"install"},
	})
	if status != http.StatusOK {
		t.Fatalf("a proven callback = %d, want 200: %s", status, landed)
	}
	var answer landedBody
	decodeInto(t, landed, &answer)
	if answer.Connect != "connected" || answer.IntegrationID == "" {
		t.Fatalf("a proven callback landed as %+v", answer)
	}

	found := plane.integrations(t, surfaceOrg)
	if len(found) != 1 {
		t.Fatalf("connecting produced %d integrations", len(found))
	}
	one := found[0]
	if one.Status != "active" {
		t.Errorf("a connected integration is %q, want active; note: %s",
			one.Status, one.VerifyNote)
	}
	if one.Configuration["installationId"] != float64(77) {
		t.Errorf("the installation is not on the record: %+v", one.Configuration)
	}
	if !strings.Contains(one.Name, "acme-corp") {
		t.Errorf("the integration is called %q and does not name the account", one.Name)
	}
	if vendor.credentialFor("/user/installations") != "Bearer "+installedToken {
		t.Errorf("the association check presented %q rather than the user access token",
			vendor.credentialFor("/user/installations"))
	}
	if vendor.credentialFor("/installation/repositories") != "Bearer ghs_minted" {
		t.Errorf("the repository read presented %q rather than an installation token",
			vendor.credentialFor("/installation/repositories"))
	}

	// Changing which repositories are selected does not go through OpenCluster, so the
	// record says where in GitHub that is done.
	facts := one.VerifyFacts
	if facts["account"] != "acme-corp" || facts["repositorySelection"] != "selected" {
		t.Errorf("the record does not say what is connected: %+v", facts)
	}
	link, _ := facts["manageUrl"].(string)
	if !strings.HasPrefix(link, vendor.URL) ||
		!strings.HasSuffix(link, "/settings/installations/77") {
		t.Errorf("the manage link %q does not address this installation in github", link)
	}
}

// The user access token proves the association and is then gone. This asserts on the stored
// record rather than on a call sequence: whatever the flow did, nothing anywhere in the
// tenant's row holds a personal credential.
func TestConnectingGitHubStoresNoUserAccessToken(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	if _, landed := plane.returnFromGitHub(t, url.Values{
		"state":           {stateOf(t, started)},
		"code":            {"the-authorization-code"},
		"installation_id": {"77"},
	}); !strings.Contains(landed, "connected") {
		t.Fatalf("connecting did not succeed: %s", landed)
	}

	found := plane.integrations(t, surfaceOrg)
	if len(found) != 1 {
		t.Fatalf("connecting produced %d integrations", len(found))
	}
	status, record := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integrations/"+found[0].ID, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the integration = %d: %s", status, record)
	}
	if strings.Contains(record, installedToken) || strings.Contains(record, "gho_") {
		t.Fatalf("a user access token survived the connection: %s", record)
	}
	if strings.Contains(record, `"credential"`) {
		t.Errorf("a github integration holds a credential it should not: %s", record)
	}
}

// A state that was never issued, one already consumed, and one belonging to another
// principal are ONE answer. Telling them apart is how a caller learns which half of a guess
// landed.
func TestEveryUnusableStateGetsTheSameAnswer(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	state := stateOf(t, started)
	good := url.Values{
		"state": {state}, "code": {"the-authorization-code"}, "installation_id": {"77"},
	}
	if _, landed := plane.returnFromGitHub(t, good); !strings.Contains(landed, "connected") {
		t.Fatalf("the first callback did not succeed: %s", landed)
	}

	replayed := status2(plane.returnFromGitHub(t, good))
	unknown := status2(plane.returnFromGitHub(t, url.Values{
		"state": {"a-state-nobody-issued"}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	}))
	if replayed.status != unknown.status || replayed.body != unknown.body {
		t.Errorf("a replayed state answers %d %q and an unknown one answers %d %q; two "+
			"answers that differ are two answers",
			replayed.status, replayed.body, unknown.status, unknown.body)
	}
	if replayed.status != http.StatusBadRequest {
		t.Errorf("an unusable state = %d, want 400: %s", replayed.status, replayed.body)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 1 {
		t.Errorf("replaying a state produced %d integrations", len(found))
	}
}

type answered struct {
	status int
	body   string
}

func status2(status int, body string) answered { return answered{status, body} }

// The callback's query is not where the tenant comes from. A callback that names the
// neighbouring organization binds to the organization the stored flow named, and the
// neighbour gets nothing.
func TestTheCallbackIgnoresAnOrganizationInItsQuery(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	status, landed := plane.returnFromGitHub(t, url.Values{
		"state":           {stateOf(t, started)},
		"code":            {"the-authorization-code"},
		"installation_id": {"77"},
		"organization":    {neighbourOrg},
		"org":             {neighbourOrg},
	})
	if status != http.StatusOK {
		t.Fatalf("a callback carrying another organization = %d: %s", status, landed)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 1 {
		t.Fatalf("the flow's own organization has %d integrations", len(found))
	}
	// The bootstrap credential reaches one organization, so the neighbour is answered 404
	// by the guard — which is also the proof that nothing was written there under a
	// credential that could not have reached it.
	status, body := plane.call(t, http.MethodGet, plane.base(neighbourOrg)+"/integrations", nil)
	if status != http.StatusNotFound {
		t.Errorf("the neighbour's listing = %d, want 404: %s", status, body)
	}
}

// Connecting the same installation again is a re-verification, not a duplicate. It is also
// exactly what a customer changing an existing installation's repositories produces.
func TestConnectingTheSameInstallationAgainReVerifiesRatherThanDuplicating(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	_, first := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
		"installation_id": {"77"}, "setup_action": {"install"},
	})
	var opened landedBody
	decodeInto(t, first, &opened)

	_, restarted := plane.startConnect(t, surfaceOrg)
	status, again := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, restarted)}, "code": {"the-authorization-code"},
		"installation_id": {"77"}, "setup_action": {"update"},
	})
	if status != http.StatusOK {
		t.Fatalf("changing an installation = %d: %s", status, again)
	}
	var second landedBody
	decodeInto(t, again, &second)
	if second.IntegrationID != opened.IntegrationID {
		t.Errorf("a changed installation landed on %s rather than the existing %s",
			second.IntegrationID, opened.IntegrationID)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 1 {
		t.Errorf("reconnecting produced %d integrations, want 1", len(found))
	}
}

// An installation the deployment's own App can no longer see reports the removal rather than
// a number to check, and connecting again after it works.
func TestReconnectingAfterARevocationSucceeds(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	_, landed := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	})
	var opened landedBody
	decodeInto(t, landed, &opened)

	vendor.uninstall()
	status, verified := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/integrations/"+opened.IntegrationID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying a revoked installation = %d: %s", status, verified)
	}
	var record integrationBody
	decodeInto(t, verified, &record)
	if record.Status != "failed" || !strings.Contains(record.VerifyNote, "no longer installed") {
		t.Errorf("a revoked installation reports %q: %s", record.Status, record.VerifyNote)
	}

	// And it keeps saying so. What tells a removed installation from an id that never
	// existed is the account the LAST verification recorded, so a failing run that dropped
	// it would make the second answer worse than the first.
	status, twice := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/integrations/"+opened.IntegrationID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying a revoked installation again = %d: %s", status, twice)
	}
	var second integrationBody
	decodeInto(t, twice, &second)
	if !strings.Contains(second.VerifyNote, "no longer installed") {
		t.Errorf("the second verification forgot which account was connected: %s",
			second.VerifyNote)
	}
	if second.VerifyFacts["account"] != "acme-corp" {
		t.Errorf("a failed verification erased what was connected: %+v", second.VerifyFacts)
	}

	// The customer installs it again and reconnects.
	vendor.mu.Lock()
	vendor.reachable = []int64{77}
	vendor.mu.Unlock()

	_, restarted := plane.startConnect(t, surfaceOrg)
	status, again := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, restarted)}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	})
	if status != http.StatusOK || !strings.Contains(again, "connected") {
		t.Fatalf("reconnecting after a revocation = %d: %s", status, again)
	}
}

// A code GitHub will not exchange binds nothing and says so in this build's own words.
func TestAnUnexchangeableCodeBindsNothing(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	status, landed := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"a-code-from-somewhere-else"},
		"installation_id": {"77"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("an unexchangeable code = %d, want 400: %s", status, landed)
	}
	if exchanged := vendor.codesExchanged(); len(exchanged) != 1 {
		t.Errorf("the refused code was exchanged %d times, want once", len(exchanged))
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Errorf("an unexchangeable code left %d integrations behind", len(found))
	}
}

// Where a console origin is configured the browser is sent back to it, carrying an outcome
// from this build's own closed vocabulary and the integration's identity — never a vendor's
// words, which on this route are somebody else's string.
func TestTheBrowserLandsBackInTheConsole(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "http://console.example.test")

	_, started := plane.startConnect(t, surfaceOrg)
	parameters := url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+plane.operator+"/operator/v1/integrations/connect/callback?"+
			parameters.Encode(), nil)
	if err != nil {
		t.Fatalf("building the callback: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+surfaceToken)
	// The redirect is the assertion, so it is not followed.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("calling the callback: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.ReadAll(response.Body)

	if response.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d, want a redirect to the console",
			response.StatusCode)
	}
	target, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("the redirect target is not a url: %v", err)
	}
	if target.Host != "console.example.test" || target.Path != "/integrations" {
		t.Errorf("the browser lands on %s rather than where the flow started",
			response.Header.Get("Location"))
	}
	if target.Query().Get("connect") != "connected" ||
		target.Query().Get("integration") == "" {
		t.Errorf("the landing carries %q", target.RawQuery)
	}
}

// A deployment that registered no installation flow offers no connect button and keeps the
// configuration form, which is the self-hosted path and is unchanged behavior.
func TestADeploymentWithNoInstallationFlowKeepsTheForm(t *testing.T) {
	vendor := newGitHubFake(t, "77")
	plane := startGitHubPlane(t, vendor)

	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, body)
	}
	var catalog struct {
		Types []struct {
			Key                 string          `json:"key"`
			SupportsConnect     bool            `json:"supportsConnect"`
			ConfigurationSchema json.RawMessage `json:"configurationSchema"`
		} `json:"types"`
	}
	decodeInto(t, body, &catalog)
	for _, entry := range catalog.Types {
		if entry.Key != "github" {
			continue
		}
		if entry.SupportsConnect {
			t.Error("a deployment with no registered installation flow offers a connect button")
		}
		if !strings.Contains(string(entry.ConfigurationSchema), "installationId") {
			t.Error("the configuration form is gone from a deployment that needs it")
		}
	}

	status, refused := plane.startConnect(t, surfaceOrg)
	if status != http.StatusBadRequest {
		t.Fatalf("starting a connect with no installation flow = %d, want 400: %s",
			status, refused)
	}
	if !strings.Contains(refused, "settings form") {
		t.Errorf("the refusal %q does not say what to do instead", refused)
	}

	// And the manual path still works, unchanged.
	if status, created := plane.createGitHub(t, "Acme GitHub", 77); status != http.StatusCreated {
		t.Fatalf("the manual path = %d, want 201: %s", status, created)
	}
}

// Where the flow IS registered, the catalog says so, and the authorization URL sends the
// browser to GitHub's own installation screen rather than to anything this product renders.
func TestTheCatalogOffersTheInstallationFlowAndSendsTheBrowserToGitHub(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, body)
	}
	if !strings.Contains(body, `"supportsConnect":true`) {
		t.Errorf("the catalog does not offer the installation flow: %s", body)
	}

	_, started := plane.startConnect(t, surfaceOrg)
	var answer struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}
	decodeInto(t, started, &answer)
	if !strings.Contains(answer.AuthorizationURL, "/apps/opencluster/installations/new") {
		t.Errorf("the browser is sent to %q rather than github's installation screen",
			answer.AuthorizationURL)
	}
}

// Nothing the flow does may put the deployment's client secret, its App key or a user
// access token into a log line.
func TestConnectingGitHubLogsNoCredential(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	if _, landed := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	}); !strings.Contains(landed, "connected") {
		t.Fatalf("connecting did not succeed: %s", landed)
	}
	// A refused one too: the refusal path is where a message is most tempting to quote.
	_, restarted := plane.startConnect(t, surfaceOrg)
	plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, restarted)}, "code": {"the-authorization-code"},
		"installation_id": {"99"},
	})

	logged := plane.logs.String()
	for _, secret := range []string{
		"the-client-secret", installedToken, "gho_", "ghs_minted",
		"BEGIN RSA PRIVATE KEY", "the-authorization-code",
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("a credential reached the log: %q appears in it", secret)
		}
	}
}

// A link somebody else started is not this caller's to finish. The flow is started honestly
// and the stored row is then made somebody else's, which is what a state captured from
// another person's browser looks like from here — and it is answered exactly as an unknown
// state is.
func TestAStateStartedByAnotherPrincipalIsRefusedIdentically(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	state := stateOf(t, started)

	reshapeFlow(t, plane.dsn,
		`UPDATE integration_connect_flow SET principal = 'somebody-else'`)

	stolen := status2(plane.returnFromGitHub(t, url.Values{
		"state": {state}, "code": {"the-authorization-code"}, "installation_id": {"77"},
	}))
	unknown := status2(plane.returnFromGitHub(t, url.Values{
		"state": {"a-state-nobody-issued"}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	}))
	if stolen.status != unknown.status || stolen.body != unknown.body {
		t.Errorf("a stolen state answers %d %q and an unknown one answers %d %q; two "+
			"answers that differ are two answers",
			stolen.status, stolen.body, unknown.status, unknown.body)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Errorf("a stolen state bound %d integrations", len(found))
	}
}

// An expired state is the same refusal too, and it is the reason the state is short-lived:
// a captured callback stops being worth anything.
func TestAnExpiredStateIsRefused(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	state := stateOf(t, started)

	// The row's own constraint keeps an expiry after its creation, so the whole flow is
	// moved back in time rather than only its deadline.
	reshapeFlow(t, plane.dsn, `UPDATE integration_connect_flow
		   SET created_at  = now() - interval '2 hours',
		       expires_at  = now() - interval '1 hour'`)

	status, landed := plane.returnFromGitHub(t, url.Values{
		"state": {state}, "code": {"the-authorization-code"}, "installation_id": {"77"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("an expired state = %d, want 400: %s", status, landed)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Errorf("an expired state bound %d integrations", len(found))
	}
}

// reshapeFlow rewrites the stored flow so a test can stand where an attacker does: holding a
// state that was issued to somebody else, or one that has run out.
func reshapeFlow(t *testing.T, dsn, statement string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	database, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to reshape the flow: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	if _, err := database.Exec(ctx, statement); err != nil {
		t.Fatalf("reshaping the flow: %v", err)
	}
}

// Starting an installation flow creates an Integration at the end of it, so it needs the
// permission that creates one. A Viewer may read the catalog and may not press Connect.
func TestAViewerCannotStartAnInstallationFlow(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlaneAs(t, vendor, "", string(authz.Viewer))

	status, refused := plane.startConnect(t, surfaceOrg)
	if status != http.StatusForbidden {
		t.Fatalf("a viewer starting a connect = %d, want 403: %s", status, refused)
	}
	if !strings.Contains(refused, string(authz.IntegrationCreate)) {
		t.Errorf("the refusal %q does not name what was missing", refused)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Errorf("a refused start produced %d integrations", len(found))
	}
}

// A company with two GitHub organizations is not forced to choose. Both connect, both are
// verified, and each names the account it belongs to.
func TestTwoGitHubAccountsCanBothBeConnected(t *testing.T) {
	vendor := newInstallFake(t, 77, 88)
	vendor.mu.Lock()
	vendor.accounts[88] = "acme-labs"
	vendor.mu.Unlock()
	plane := startInstallPlane(t, vendor, "")

	for _, installation := range []string{"77", "88"} {
		_, started := plane.startConnect(t, surfaceOrg)
		status, landed := plane.returnFromGitHub(t, url.Values{
			"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
			"installation_id": {installation},
		})
		if status != http.StatusOK || !strings.Contains(landed, "connected") {
			t.Fatalf("connecting installation %s = %d: %s", installation, status, landed)
		}
	}

	found := plane.integrations(t, surfaceOrg)
	if len(found) != 2 {
		t.Fatalf("two accounts produced %d integrations", len(found))
	}
	names := map[string]bool{}
	for _, one := range found {
		if one.Status != "active" {
			t.Errorf("%q is %q, want active; note: %s", one.Name, one.Status, one.VerifyNote)
		}
		names[one.Name] = true
	}
	if !names["GitHub — acme-corp"] || !names["GitHub — acme-labs"] {
		t.Errorf("the two integrations are named %v and do not name their accounts", names)
	}
}

// Reinstalling on the same account gives the same suggested name under a different
// installation. The second one is disambiguated rather than refused, and neither name is
// cut mid-rune — the suggested name carries an em dash.
func TestReinstallingOnOneAccountDoesNotCollideOnTheName(t *testing.T) {
	vendor := newInstallFake(t, 77, 88)
	plane := startInstallPlane(t, vendor, "")

	for _, installation := range []string{"77", "88"} {
		_, started := plane.startConnect(t, surfaceOrg)
		if status, landed := plane.returnFromGitHub(t, url.Values{
			"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
			"installation_id": {installation},
		}); status != http.StatusOK {
			t.Fatalf("connecting installation %s = %d: %s", installation, status, landed)
		}
	}

	found := plane.integrations(t, surfaceOrg)
	if len(found) != 2 {
		t.Fatalf("two installations on one account produced %d integrations", len(found))
	}
	for _, one := range found {
		if !utf8.ValidString(one.Name) {
			t.Errorf("the name %q is not valid UTF-8", one.Name)
		}
		if !strings.HasPrefix(one.Name, "GitHub — acme-corp") {
			t.Errorf("the name %q lost the account it belongs to", one.Name)
		}
	}
	if found[0].Name == found[1].Name {
		t.Errorf("both installations are called %q", found[0].Name)
	}
}

// Disconnecting removes reach: a disabled integration is not offered to an investigation,
// and a deleted one is gone. The record of what it produced is what makes deletion refusable
// — with nothing depending on it, removal is one action.
func TestDisconnectingGitHubRemovesReach(t *testing.T) {
	vendor := newInstallFake(t, 77)
	plane := startInstallPlane(t, vendor, "")

	_, started := plane.startConnect(t, surfaceOrg)
	_, landed := plane.returnFromGitHub(t, url.Values{
		"state": {stateOf(t, started)}, "code": {"the-authorization-code"},
		"installation_id": {"77"},
	})
	var opened landedBody
	decodeInto(t, landed, &opened)
	address := plane.base(surfaceOrg) + "/integrations/" + opened.IntegrationID

	status, body := plane.call(t, http.MethodPost, address+"/enabled",
		map[string]any{"enabled": false})
	if status != http.StatusNoContent && status != http.StatusOK {
		t.Fatalf("disabling = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodGet, address, nil)
	if status != http.StatusOK {
		t.Fatalf("reading a disabled integration = %d: %s", status, body)
	}
	var disabled integrationBody
	decodeInto(t, body, &disabled)
	if !disabled.Disabled {
		t.Error("a disabled integration does not say it is disabled")
	}

	if status, body = plane.call(t, http.MethodDelete, address, nil); status != http.StatusNoContent {
		t.Fatalf("deleting = %d, want 204: %s", status, body)
	}
	if status, body = plane.call(t, http.MethodGet, address, nil); status != http.StatusNotFound {
		t.Errorf("reading a deleted integration = %d, want 404: %s", status, body)
	}
	if found := plane.integrations(t, surfaceOrg); len(found) != 0 {
		t.Errorf("disconnecting left %d integrations behind", len(found))
	}
}

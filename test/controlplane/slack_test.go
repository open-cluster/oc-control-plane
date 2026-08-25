package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

// The Slack surface at the composition seam: the assembled process, a real database, and
// a fake vendor standing where slack.com would. What is asserted is what an operator and
// a security reviewer care about — a pasted token is verified live before saving, stored
// sealed, never echoed; verification re-probes the real far end; and a deployment without
// a sealing key refuses to serve a credential-bearing catalog at all.

// vendorFake is the minimal Slack the composition tests need: auth.test, judging the
// presented bearer token against what the fake currently accepts.
type vendorFake struct {
	*httptest.Server

	mu sync.Mutex
	// accepts is the one token auth.test answers ok to; everything else is invalid_auth.
	accepts string
	// scopes is what an accepted token is granted.
	scopes string
	// channels, when set, is what conversations.list answers, for the tests that read.
	channels string
	// authCalls counts auth.test calls, so a test can prove the probe happened.
	authCalls int

	// knownCode is the one authorization code the fake will exchange; anything else is
	// refused, which is how a replayed or invented code is exercised.
	knownCode string
	// team is the workspace the exchange reports installing into. Two tests need two
	// workspaces to prove that reconnecting one re-verifies rather than duplicating.
	team string
	// exchanged records every code the fake was asked to exchange, so a test can prove
	// the exchange happened server-side and happened once.
	exchanged []string
	// exchangeSecret records the client secret the exchange was presented with, so a test
	// can prove it went in the body rather than through a browser.
	exchangeSecret string
}

// exchange answers oauth.v2.access: the authorization code for the workspace's bot token.
func (f *vendorFake) exchange(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	code, known, team := request.PostFormValue("code"), f.knownCode, f.team
	f.exchanged = append(f.exchanged, code)
	f.exchangeSecret = request.PostFormValue("client_secret")
	accepts, scopes := f.accepts, f.scopes
	f.mu.Unlock()

	if code == "" || code != known {
		_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_code"}`))
		return
	}
	_, _ = writer.Write([]byte(`{"ok":true,"access_token":"` + accepts +
		`","token_type":"bot","scope":"` + scopes +
		`","app_id":"A0OPENCLUSTER","bot_user_id":"U0BOT","team":{"id":"` + team +
		`","name":"Acme"},"is_enterprise_install":false,` +
		`"authed_user":{"id":"U0ADMIN"}}`))
}

// codesExchanged reports every code the fake was asked to exchange.
func (f *vendorFake) codesExchanged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.exchanged...)
}

func newVendorFake(t *testing.T, accepts string) *vendorFake {
	t.Helper()
	fake := &vendorFake{accepts: accepts,
		scopes:    "channels:read,channels:history,search:read,users:read",
		knownCode: "the-authorization-code",
		team:      "T0ACME"}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			fake.mu.Lock()
			accepted := request.Header.Get("Authorization") == "Bearer "+fake.accepts
			scopes, channels := fake.scopes, fake.channels
			if request.URL.Path == "/auth.test" {
				fake.authCalls++
			}
			fake.mu.Unlock()

			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.URL.Path == "/oauth.v2.access":
				// Before the bearer check: an authorization code exchange presents the
				// app's client credential in the body, not a workspace token.
				fake.exchange(writer, request)
			case !accepted:
				_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			case request.URL.Path == "/auth.test":
				writer.Header().Set("X-OAuth-Scopes", scopes)
				_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme",` +
					`"user":"opencluster-bot","url":"https://acme.slack.com/"}`))
			case request.URL.Path == "/conversations.list" && channels != "":
				_, _ = writer.Write([]byte(channels))
			default:
				t.Errorf("the fake vendor was asked for %q", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(fake.Close)
	return fake
}

// serveChannels teaches the fake a conversations.list answer.
func (f *vendorFake) serveChannels(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = body
}

func (f *vendorFake) accept(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepts = token
}

func (f *vendorFake) grant(scopes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = scopes
}

func (f *vendorFake) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCalls
}

// startSlackPlane starts a control plane whose Slack provider reaches the fake vendor.
func startSlackPlane(t *testing.T, vendor *vendorFake) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.SlackAPIURL = vendor.URL
	})
	return &integrationPlane{controlPlane: plane, operator: operatorAddress}
}

func (p *integrationPlane) createSlack(t *testing.T, name, token string) (int, string) {
	t.Helper()
	return p.call(t, http.MethodPost, p.base(surfaceOrg)+"/integrations", map[string]any{
		"type":          "slack",
		"name":          name,
		"configuration": map[string]any{"botToken": token},
	})
}

func TestSlackCreateVerifiesLiveBeforeSaving(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)
	base := plane.base(surfaceOrg)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("creating with a working token = %d: %s", status, body)
	}
	if vendor.probes() == 0 {
		t.Fatal("the integration saved without the vendor ever being asked; " +
			"\"verified\" would rest on a form having validated")
	}
	var created createdBody
	decodeInto(t, body, &created)
	if created.Integration.Status != "active" {
		t.Errorf("a live-verified integration is %q, want active; note: %s",
			created.Integration.Status, created.Integration.VerifyNote)
	}
	if !strings.Contains(created.Integration.VerifyNote, "Acme") {
		t.Errorf("the note %q does not name the workspace that answered",
			created.Integration.VerifyNote)
	}
	if created.WebhookSecret != "" {
		t.Error("a slack integration was handed a webhook secret it cannot use")
	}

	t.Run("the token is nowhere in any later answer", func(t *testing.T) {
		for _, address := range []string{
			base + "/integrations/" + created.Integration.ID,
			base + "/integrations",
			base + "/integration-types",
		} {
			status, answer := plane.call(t, http.MethodGet, address, nil)
			if status != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", address, status, answer)
			}
			if strings.Contains(answer, "xoxb-good-token-1234") {
				t.Fatalf("the pasted token appears in %s; it must be write-only after entry",
					address)
			}
		}
	})

	t.Run("the read shows credential identity, not the credential", func(t *testing.T) {
		status, answer := plane.call(t, http.MethodGet,
			base+"/integrations/"+created.Integration.ID, nil)
		if status != http.StatusOK {
			t.Fatalf("reading back = %d: %s", status, answer)
		}
		var read struct {
			Credential *struct {
				Fingerprint string `json:"fingerprint"`
				CreatedAt   string `json:"createdAt"`
			} `json:"credential"`
			Configuration map[string]any `json:"configuration"`
		}
		decodeInto(t, answer, &read)
		if read.Credential == nil || read.Credential.Fingerprint == "" {
			t.Errorf("no credential identity; an operator cannot tell one token from the next: %s", answer)
		}
		if _, leaked := read.Configuration["botToken"]; leaked {
			t.Error("the token reached configuration; a secret never lives in that column")
		}
	})
}

func TestSlackCreateWithARefusedTokenSavesNothing(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-the-only-good-token")
	plane := startSlackPlane(t, vendor)
	base := plane.base(surfaceOrg)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-a-typo")
	if status != http.StatusBadRequest {
		t.Fatalf("a refused token = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, "invalid_auth") {
		t.Errorf("the refusal %q does not carry the vendor's own reason", body)
	}

	status, listing := plane.call(t, http.MethodGet, base+"/integrations", nil)
	if status != http.StatusOK {
		t.Fatalf("listing = %d: %s", status, listing)
	}
	var listed struct {
		Items []integrationBody `json:"items"`
	}
	decodeInto(t, listing, &listed)
	if len(listed.Items) != 0 {
		t.Errorf("a failed setup left %d integrations behind; a typo must fail at setup, "+
			"not during the next incident", len(listed.Items))
	}
}

func TestSlackVerifyReProbesTheRealFarEnd(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)
	base := plane.base(surfaceOrg)

	_, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	var created createdBody
	decodeInto(t, body, &created)

	// The workspace admin revokes the token at the vendor. Nothing in this database
	// changed, so only a real probe can notice.
	vendor.accept("xoxb-a-newer-token")

	status, answer := plane.call(t, http.MethodPost,
		base+"/integrations/"+created.Integration.ID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying = %d: %s", status, answer)
	}
	var verified integrationBody
	decodeInto(t, answer, &verified)
	if verified.Status != "failed" {
		t.Errorf("a revoked token verifies as %q, want failed; note: %s",
			verified.Status, verified.VerifyNote)
	}
	if !strings.Contains(verified.VerifyNote, "invalid_auth") {
		t.Errorf("the note %q does not say what the vendor said", verified.VerifyNote)
	}
}

func TestSlackMissingScopesSurfaceAsDegraded(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.grant("channels:read,channels:history")
	plane := startSlackPlane(t, vendor)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("a valid token with narrow scopes must still save = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	if created.Integration.Status != "degraded" {
		t.Errorf("status = %q, want degraded; note: %s",
			created.Integration.Status, created.Integration.VerifyNote)
	}
	if !strings.Contains(created.Integration.VerifyNote, "users:read") {
		t.Errorf("the note %q does not name the missing scope users:read",
			created.Integration.VerifyNote)
	}
	// search:read is NOT named. Degraded means a capability this integration was
	// configured to provide is failing, and workspace-wide search was never asked for —
	// listing it here is what made every correct installation look broken.
	if strings.Contains(created.Integration.VerifyNote, "search:read") {
		t.Errorf("the note %q holds a scope this product never requests",
			created.Integration.VerifyNote)
	}
}

func TestSlackPatchReplacesTheCredentialWriteOnly(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-first-token-1234")
	plane := startSlackPlane(t, vendor)
	base := plane.base(surfaceOrg)

	_, body := plane.createSlack(t, "Acme Slack", "xoxb-first-token-1234")
	var created createdBody
	decodeInto(t, body, &created)

	firstFingerprint := credentialFingerprint(t, body)

	// The rotation at the vendor: the old token dies, a new one is issued and pasted.
	vendor.accept("xoxb-second-token-5678")

	status, answer := plane.call(t, http.MethodPatch,
		base+"/integrations/"+created.Integration.ID, map[string]any{
			"configuration": map[string]any{"botToken": "xoxb-second-token-5678"},
		})
	if status != http.StatusOK {
		t.Fatalf("replacing the credential = %d: %s", status, answer)
	}
	if strings.Contains(answer, "xoxb-second-token-5678") {
		t.Fatal("the replacement token was echoed back")
	}
	var revised integrationBody
	decodeInto(t, answer, &revised)
	if revised.Status != "active" {
		t.Errorf("a replaced-and-verified credential reads %q, want active; note: %s",
			revised.Status, revised.VerifyNote)
	}
	if next := credentialFingerprint(t, answer); next == "" || next == firstFingerprint {
		t.Error("the credential identity did not change; an operator cannot see the replacement")
	}

	t.Run("a replacement the vendor refuses changes nothing", func(t *testing.T) {
		status, answer := plane.call(t, http.MethodPatch,
			base+"/integrations/"+created.Integration.ID, map[string]any{
				"configuration": map[string]any{"botToken": "xoxb-pasted-wrong"},
			})
		if status != http.StatusBadRequest {
			t.Fatalf("a refused replacement = %d, want 400: %s", status, answer)
		}

		status, verifyAnswer := plane.call(t, http.MethodPost,
			base+"/integrations/"+created.Integration.ID+"/verify", nil)
		if status != http.StatusOK {
			t.Fatalf("verifying after the refused replacement = %d: %s", status, verifyAnswer)
		}
		var verified integrationBody
		decodeInto(t, verifyAnswer, &verified)
		if verified.Status != "active" {
			t.Errorf("the stored credential should still be the working one, got %q; note: %s",
				verified.Status, verified.VerifyNote)
		}
	})
}

func TestSlackCatalogEntryRendersTheToolsAndTheWriteOnlySchema(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)

	status, body := plane.call(t, http.MethodGet,
		plane.base(surfaceOrg)+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, body)
	}
	var catalog struct {
		Types []struct {
			Key                 string          `json:"key"`
			ConfigurationSchema json.RawMessage `json:"configurationSchema"`
			Tools               []struct {
				Name         string `json:"name"`
				WhenToUse    string `json:"whenToUse"`
				WhenNotToUse string `json:"whenNotToUse"`
			} `json:"tools"`
		} `json:"types"`
	}
	decodeInto(t, body, &catalog)

	for _, entry := range catalog.Types {
		if entry.Key != "slack" {
			continue
		}
		if len(entry.Tools) != 4 {
			t.Errorf("slack serves %d tools, want 4", len(entry.Tools))
		}
		for _, tool := range entry.Tools {
			if tool.WhenToUse == "" || tool.WhenNotToUse == "" {
				t.Errorf("tool %s is rendered without its routing guidance", tool.Name)
			}
		}
		if !strings.Contains(string(entry.ConfigurationSchema), `"writeOnly":true`) {
			t.Error("the schema does not mark the token write-only")
		}
		return
	}
	t.Fatalf("the catalog does not serve slack: %s", body)
}

func TestSlackAnotherTenantSeesNothing(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	plane := startSlackPlane(t, vendor)

	_, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	var created createdBody
	decodeInto(t, body, &created)

	status, answer := plane.call(t, http.MethodGet,
		plane.base(neighbourOrg)+"/integrations/"+created.Integration.ID, nil)
	if status != http.StatusNotFound {
		t.Errorf("a neighbour reading this tenant's slack integration = %d, want 404: %s",
			status, answer)
	}
}

// A deployment whose catalog holds a credential-bearing type and whose configuration
// names no sealing key must refuse to serve the operator surface: the alternative is a
// setup flow that accepts a token it can only store in the clear or drop.
func TestRunRefusesACredentialCatalogWithoutASealingKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	digest := sha256.Sum256([]byte(surfaceToken))
	cfg := config.Config{
		HTTPAddress:               "127.0.0.1:0",
		DatabaseDSN:               freshDatabase(t),
		ShutdownTimeout:           5 * time.Second,
		ServiceName:               "oc-control-plane-test",
		OperatorAddress:           freeAddress(t),
		OperatorTokenDigest:       digest[:],
		OperatorTokenOrganization: surfaceOrg,
		SealingKey:                nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := app.Run(ctx, cfg, io.Discard, app.Options{})
	if err == nil {
		t.Fatal("the process served a credential-bearing catalog with no way to seal a credential")
	}
	if !strings.Contains(err.Error(), config.EnvSealingKeyFile) {
		t.Errorf("the refusal %q does not name the variable to set", err.Error())
	}
}

// credentialFingerprint digs the credential identity out of either response shape.
func credentialFingerprint(t *testing.T, body string) string {
	t.Helper()
	var shapes struct {
		Credential *struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"credential"`
		Integration *struct {
			Credential *struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"credential"`
		} `json:"integration"`
	}
	decodeInto(t, body, &shapes)
	if shapes.Credential != nil {
		return shapes.Credential.Fingerprint
	}
	if shapes.Integration != nil && shapes.Integration.Credential != nil {
		return shapes.Integration.Credential.Fingerprint
	}
	return ""
}

// The correction this release exists for. A bot token holding every scope OpenCluster asks
// for is a CORRECT installation, and it used to report degraded because it lacked
// workspace-wide search — a permission this product deliberately declines to request. The
// customer was being told to fix something that was not broken.
func TestSlackRecommendedBotInstallationIsActiveWithSearchUnavailable(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackPlane(t, vendor)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("creating with the recommended scopes = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	if created.Integration.Status != "active" {
		t.Fatalf("status = %q, want active — a correct installation must not report "+
			"itself broken; note: %s",
			created.Integration.Status, created.Integration.VerifyNote)
	}

	// Unavailable, not missing. Its absence is a stated choice, and saying so is what
	// stops an operator going looking for a permission to grant.
	search := created.Integration.tool(t, "slack.search_messages")
	if search.Available {
		t.Error("workspace-wide search reads as available on a token that was never " +
			"granted it")
	}
	if search.Reason == "" {
		t.Error("search is unavailable and says nothing about why")
	}

	// The Tools the installation does have are reported as working, individually,
	// so an operator can see what OpenCluster can and cannot read.
	for _, working := range []string{"slack.list_channels", "slack.get_channel_history"} {
		if reported := created.Integration.tool(t, working); !reported.Available {
			t.Errorf("%s has its scope and reads as unavailable: %+v", working, reported)
		}
	}
}

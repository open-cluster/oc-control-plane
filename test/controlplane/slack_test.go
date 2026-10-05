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

type vendorFake struct {
	*httptest.Server

	mu        sync.Mutex
	accepts   string
	scopes    string
	channels  string
	authCalls int

	knownCode      string
	team           string
	exchanged      []string
	exchangeSecret string
}

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

func startSlackPlane(t *testing.T, vendor *vendorFake) *integrationPlane {
	t.Helper()

	apiAddress := freeAddress(t)
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		dsn = cfg.DatabaseDSN
	}, app.Options{SlackAPIURL: vendor.URL})
	return &integrationPlane{controlPlane: plane, api: apiAddress, dsn: dsn}
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
	if created.Integration.Status != "verified" {
		t.Errorf("a live-verified integration is %q, want verified; note: %s",
			created.Integration.Status, created.Integration.VerificationNote)
	}
	if !strings.Contains(created.Integration.VerificationNote, "Acme") {
		t.Errorf("the note %q does not name the workspace that answered",
			created.Integration.VerificationNote)
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
				Configured bool `json:"configured"`
			} `json:"credential"`
			Configuration map[string]any `json:"configuration"`
		}
		decodeInto(t, answer, &read)
		if read.Credential == nil || !read.Credential.Configured {
			t.Errorf("the response does not report a configured credential: %s", answer)
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
			verified.Status, verified.VerificationNote)
	}
	if !strings.Contains(verified.VerificationNote, "invalid_auth") {
		t.Errorf("the note %q does not say what the vendor said", verified.VerificationNote)
	}
}

func TestSlackMissingScopesLimitTools(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.grant("channels:read,channels:history")
	plane := startSlackPlane(t, vendor)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("a valid token with narrow scopes must still save = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	if created.Integration.Status != "verified" {
		t.Errorf("status = %q, want verified; note: %s",
			created.Integration.Status, created.Integration.VerificationNote)
	}
	if !strings.Contains(created.Integration.VerificationNote, "users:read") {
		t.Errorf("the note %q does not name the missing scope users:read",
			created.Integration.VerificationNote)
	}
	if strings.Contains(created.Integration.VerificationNote, "search:read") {
		t.Errorf("the note %q holds a scope this product never requests",
			created.Integration.VerificationNote)
	}
}

func TestSlackPatchReplacesTheCredentialWriteOnly(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-first-token-1234")
	plane := startSlackPlane(t, vendor)
	base := plane.base(surfaceOrg)

	_, body := plane.createSlack(t, "Acme Slack", "xoxb-first-token-1234")
	var created createdBody
	decodeInto(t, body, &created)

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
	if revised.Status != "verified" {
		t.Errorf("a replaced-and-verified credential reads %q, want verified; note: %s",
			revised.Status, revised.VerificationNote)
	}
	if revised.Credential == nil || !revised.Credential.Configured {
		t.Error("the replacement credential is not configured")
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
		if verified.Status != "verified" {
			t.Errorf("the stored credential should still be the working one, got %q; note: %s",
				verified.Status, verified.VerificationNote)
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
			Key                 string            `json:"key"`
			ConfigurationSchema json.RawMessage   `json:"configurationSchema"`
			Tools               []json.RawMessage `json:"tools"`
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
		for _, raw := range entry.Tools {
			var tool map[string]json.RawMessage
			if err := json.Unmarshal(raw, &tool); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"name", "description"} {
				if len(tool[field]) == 0 {
					t.Errorf("tool is rendered without %s: %s", field, raw)
				}
			}
			for _, removed := range []string{"whenToUse", "whenNotToUse", "permissions", "output"} {
				if _, present := tool[removed]; present {
					t.Errorf("tool still renders obsolete %s: %s", removed, raw)
				}
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
	defer plane.switchOrganization(t, neighbourOrg)()

	status, answer := plane.call(t, http.MethodGet,
		plane.base(neighbourOrg)+"/integrations/"+created.Integration.ID, nil)
	if status != http.StatusNotFound {
		t.Errorf("a neighbour reading this tenant's slack integration = %d, want 404: %s",
			status, answer)
	}
}

func TestRunRefusesACredentialCatalogWithoutASealingKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	digest := sha256.Sum256([]byte(surfaceToken))
	cfg := config.Config{
		DatabaseDSN:          freshDatabase(t),
		HTTPListenAddress:    freeAddress(t),
		BootstrapTokenDigest: digest[:],
		SealingKey:           nil,
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

func TestSlackRecommendedBotInstallationIsVerifiedWithSearchUnavailable(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-good-token-1234")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackPlane(t, vendor)

	status, body := plane.createSlack(t, "Acme Slack", "xoxb-good-token-1234")
	if status != http.StatusCreated {
		t.Fatalf("creating with the recommended scopes = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	if created.Integration.Status != "verified" {
		t.Fatalf("status = %q, want verified — a correct installation must not report "+
			"itself broken; note: %s",
			created.Integration.Status, created.Integration.VerificationNote)
	}

	search := created.Integration.tool(t, "slack.search_messages")
	if search.Available {
		t.Error("workspace-wide search reads as available on a token that was never " +
			"granted it")
	}
	if search.Reason == "" {
		t.Error("search is unavailable and says nothing about why")
	}

	for _, working := range []string{"slack.list_channels", "slack.get_channel_history"} {
		if reported := created.Integration.tool(t, working); !reported.Available {
			t.Errorf("%s has its scope and reads as unavailable: %+v", working, reported)
		}
	}
}

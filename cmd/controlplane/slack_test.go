package main

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
	// authCalls counts auth.test calls, so a test can prove the probe happened.
	authCalls int
}

func newVendorFake(t *testing.T, accepts string) *vendorFake {
	t.Helper()
	fake := &vendorFake{accepts: accepts, scopes: "channels:read,channels:history,search:read"}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/auth.test" {
				t.Errorf("the fake vendor was asked for %q", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			fake.mu.Lock()
			fake.authCalls++
			accepted := request.Header.Get("Authorization") == "Bearer "+fake.accepts
			scopes := fake.scopes
			fake.mu.Unlock()

			writer.Header().Set("Content-Type", "application/json")
			if !accepted {
				_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
				return
			}
			writer.Header().Set("X-OAuth-Scopes", scopes)
			_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme","user":"opencluster-bot"}`))
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

// startSlackPlane starts a control plane whose Slack provider reaches the fake vendor.
func startSlackPlane(t *testing.T, vendor *vendorFake) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.Assignments[neighbourOrg] = "shared"
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
	if !strings.Contains(created.Integration.VerifyNote, "search:read") {
		t.Errorf("the note %q does not name the missing scope", created.Integration.VerifyNote)
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
			Capabilities        []string        `json:"capabilities"`
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
		if len(entry.Capabilities) != 4 || len(entry.Tools) != 4 {
			t.Errorf("slack serves %d capabilities and %d tools, want 4 and 4",
				len(entry.Capabilities), len(entry.Tools))
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
		Placements:                map[string]string{"shared": freshDatabase(t)},
		Assignments:               map[string]string{surfaceOrg: "shared"},
		ShutdownTimeout:           5 * time.Second,
		ServiceName:               "oc-control-plane-test",
		OperatorAddress:           freeAddress(t),
		OperatorTokenDigest:       digest[:],
		OperatorTokenOrganization: surfaceOrg,
		SealingKey:                nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := run(ctx, cfg, io.Discard, wiring{})
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

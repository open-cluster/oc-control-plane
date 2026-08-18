package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

// The GitHub surface at the composition seam: the assembled process, a real database, and
// a fake vendor standing where api.github.com would. The App credential is deployment
// configuration; what an operator submits is an installation id, and "verified" means the
// installation answered — with its account identity and the repositories it granted.

// githubFake is the minimal GitHub the composition tests need: one installation, its
// token mint, and its repository grant.
type githubFake struct {
	*httptest.Server

	mu sync.Mutex
	// installation is the one id the fake knows; everything else is a 404.
	installation string
	suspended    bool
}

func newGitHubFake(t *testing.T, installation string) *githubFake {
	t.Helper()
	fake := &githubFake{installation: installation}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			fake.mu.Lock()
			known, suspended := fake.installation, fake.suspended
			fake.mu.Unlock()

			switch {
			case request.URL.Path == "/app/installations/"+known:
				suspendedAt := "null"
				if suspended {
					suspendedAt = `"2026-08-01T00:00:00Z"`
				}
				_, _ = writer.Write([]byte(`{"id":` + known + `,
					"account":{"login":"acme-corp","type":"Organization"},
					"repository_selection":"selected","suspended_at":` + suspendedAt + `}`))
			case request.URL.Path == "/app/installations/"+known+"/access_tokens":
				writer.WriteHeader(http.StatusCreated)
				_, _ = writer.Write([]byte(
					`{"token":"ghs_minted","expires_at":"2036-01-01T00:00:00Z"}`))
			case request.URL.Path == "/installation/repositories":
				_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
					{"id":1296269,"name":"payments","full_name":"acme-corp/payments"},
					{"id":1296270,"name":"deploy","full_name":"acme-corp/deploy"}]}`))
			case strings.HasPrefix(request.URL.Path, "/app/installations/"):
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
			default:
				t.Errorf("the fake vendor was asked for %q", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *githubFake) suspend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = true
}

// appKeyPEM generates a deployment App key for one test plane.
func appKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// startGitHubPlane starts a control plane whose GitHub provider reaches the fake vendor.
func startGitHubPlane(t *testing.T, vendor *githubFake) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.Assignments[neighbourOrg] = "shared"
		cfg.GitHubAppID = "12345"
		cfg.GitHubAppKey = appKeyPEM(t)
		cfg.GitHubAPIURL = vendor.URL
	})
	return &integrationPlane{controlPlane: plane, operator: operatorAddress}
}

func (p *integrationPlane) createGitHub(t *testing.T, name string, installation int) (int, string) {
	t.Helper()
	return p.call(t, http.MethodPost, p.base(surfaceOrg)+"/integrations", map[string]any{
		"type":          "github",
		"name":          name,
		"configuration": map[string]any{"installationId": installation},
	})
}

func TestGitHubCreateVerifiesTheInstallationLive(t *testing.T) {
	vendor := newGitHubFake(t, "77")
	plane := startGitHubPlane(t, vendor)

	status, body := plane.createGitHub(t, "Acme GitHub", 77)
	if status != http.StatusCreated {
		t.Fatalf("creating with a live installation = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	if created.Integration.Status != "active" {
		t.Errorf("a live-verified installation is %q, want active; note: %s",
			created.Integration.Status, created.Integration.VerifyNote)
	}
	if !strings.Contains(created.Integration.VerifyNote, "acme-corp") {
		t.Errorf("the note %q does not name the account that answered",
			created.Integration.VerifyNote)
	}
	if created.WebhookSecret != "" {
		t.Error("a github integration was handed a webhook secret it cannot use")
	}
	if created.Integration.Configuration["installationId"] != float64(77) {
		t.Errorf("the installation id is not on the record: %+v",
			created.Integration.Configuration)
	}
}

func TestGitHubCreateWithAnUnknownInstallationSavesNothing(t *testing.T) {
	vendor := newGitHubFake(t, "77")
	plane := startGitHubPlane(t, vendor)
	base := plane.base(surfaceOrg)

	status, body := plane.createGitHub(t, "Acme GitHub", 404404)
	if status != http.StatusBadRequest {
		t.Fatalf("an unknown installation = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, "does not know installation") {
		t.Errorf("the refusal %q does not say what github said", body)
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
		t.Errorf("a failed setup left %d integrations behind", len(listed.Items))
	}
}

func TestGitHubVerifyNoticesASuspension(t *testing.T) {
	vendor := newGitHubFake(t, "77")
	plane := startGitHubPlane(t, vendor)
	base := plane.base(surfaceOrg)

	_, body := plane.createGitHub(t, "Acme GitHub", 77)
	var created createdBody
	decodeInto(t, body, &created)

	// The customer suspends the app in GitHub. Nothing in this database changed, so only
	// a real probe can notice.
	vendor.suspend()

	status, answer := plane.call(t, http.MethodPost,
		base+"/integrations/"+created.Integration.ID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying = %d: %s", status, answer)
	}
	var verified integrationBody
	decodeInto(t, answer, &verified)
	if verified.Status != "failed" || !strings.Contains(verified.VerifyNote, "suspended") {
		t.Errorf("a suspended installation verifies as %q with note %q",
			verified.Status, verified.VerifyNote)
	}
}

// A deployment that configured no App still serves github in the catalog — the compiled
// provider set and the seeded reference rows must agree exactly — but connecting fails at
// setup with the reason, not later.
func TestGitHubWithoutAnAppRefusesAtSetup(t *testing.T) {
	operatorAddress := freeAddress(t)
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
	})
	surface := &integrationPlane{controlPlane: plane, operator: operatorAddress}

	status, body := surface.createGitHub(t, "Acme GitHub", 77)
	if status != http.StatusBadRequest {
		t.Fatalf("connecting github on a deployment with no app = %d, want 400: %s",
			status, body)
	}
	if !strings.Contains(body, "GitHub App") {
		t.Errorf("the refusal %q does not name what the deployment lacks", body)
	}

	status, catalog := surface.call(t, http.MethodGet,
		surface.base(surfaceOrg)+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, catalog)
	}
	if !strings.Contains(catalog, `"key":"github"`) {
		t.Error("github left the catalog because the deployment lacks an app; the " +
			"catalog and the seeded rows must agree exactly")
	}
}

func TestGitHubAnotherTenantSeesNothing(t *testing.T) {
	vendor := newGitHubFake(t, "77")
	plane := startGitHubPlane(t, vendor)

	_, body := plane.createGitHub(t, "Acme GitHub", 77)
	var created createdBody
	decodeInto(t, body, &created)

	status, answer := plane.call(t, http.MethodGet,
		plane.base(neighbourOrg)+"/integrations/"+created.Integration.ID, nil)
	if status != http.StatusNotFound {
		t.Errorf("a neighbour reading this tenant's github integration = %d, want 404: %s",
			status, answer)
	}
}

func TestGitHubCatalogEntrySchemaTakesTheInstallationID(t *testing.T) {
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
			Capabilities        []string        `json:"capabilities"`
			ConfigurationSchema json.RawMessage `json:"configurationSchema"`
			Tools               []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"types"`
	}
	decodeInto(t, body, &catalog)

	for _, entry := range catalog.Types {
		if entry.Key != "github" {
			continue
		}
		if len(entry.Capabilities) != 3 || len(entry.Tools) != 3 {
			t.Errorf("github serves %d capabilities and %d tools, want 3 and 3",
				len(entry.Capabilities), len(entry.Tools))
		}
		if !strings.Contains(string(entry.ConfigurationSchema), "installationId") {
			t.Error("the schema does not take the installation id")
		}
		if strings.Contains(string(entry.ConfigurationSchema), "writeOnly") {
			t.Error("an installation id is not a secret and must not render write-only")
		}
		return
	}
	t.Fatalf("the catalog does not serve github: %s", body)
}

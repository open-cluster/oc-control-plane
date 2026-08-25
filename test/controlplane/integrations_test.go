package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

// The Integration surface, asserted at the seam every slice uses: the assembled process,
// real HTTP, a real database. What is asserted is what an operator could observe — the
// catalog says what this build serves, a created Integration can actually receive a
// delivery, the webhook secret is shown once and never again, and a request naming another
// tenant is refused as if nothing existed.

const (
	surfaceToken   = "operator-token-for-the-integration-surface"
	surfaceOrg     = "org-a"
	neighbourOrg   = "org-neighbour"
	alertmanagerAt = "2026-01-02T15:04:05Z"
)

// integrationPlane is a control plane with the operator surface and intake both listening,
// plus an enrolled relay for a kubernetes Integration to bind to.
type integrationPlane struct {
	*controlPlane
	operator string
	intake   string
	relayAt  string
	relay    relayCredentials
	dsn      string
}

func startIntegrationPlane(t *testing.T) *integrationPlane {
	t.Helper()
	return startIntegrationPlaneWithOptions(t, app.Options{})
}

func startIntegrationPlaneWithOptions(t *testing.T, options app.Options) *integrationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	relayAddress := freeAddress(t)
	intakeAddress := operatorAddress
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		// The bootstrap credential is bound to ONE organization. A request naming the
		// neighbour below is refused by the authorization middleware before it reaches a
		// query — the cross-tenant assertions assert that refusal.
		cfg.OperatorTokenOrganization = surfaceOrg
		// The neighbour shares this database deliberately. An organization with no
		// database fails before any query runs, which would leave the cross-tenant
		// assertions passing against an implementation with no scoping at all.
		dsn = cfg.DatabaseDSN
	}, options)

	connection := dialRelay(t, relayAddress)
	return &integrationPlane{
		controlPlane: plane,
		operator:     operatorAddress,
		intake:       intakeAddress,
		relayAt:      relayAddress,
		relay:        registerRelay(t, connection, dsn, surfaceOrg),
		dsn:          dsn,
	}
}

func (p *integrationPlane) base(organization string) string {
	return "http://" + p.operator + "/operator/v1/organizations/" + organization
}

// call sends an authenticated operator request with an optional JSON body.
func (p *integrationPlane) call(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+surfaceToken)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response from %s: %v", url, err)
	}
	return response.StatusCode, string(answer)
}

// deliver posts a webhook body to intake with the given secret.
func (p *integrationPlane) deliver(
	t *testing.T, integrationID, secret string, payload []byte,
) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	url := "http://" + p.intake + "/intake/v1/integrations/" + integrationID + "/signals"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building the delivery: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if secret != "" {
		request.Header.Set(intake.TokenHeader, secret)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering to %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the delivery answer: %v", err)
	}
	return response.StatusCode, string(answer)
}

func decodeInto(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
}

type integrationBody struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	Disabled      bool              `json:"disabled"`
	Configuration map[string]any    `json:"configuration"`
	Labels        map[string]string `json:"labels"`
	RelayID       string            `json:"relayId"`
	Webhook       *struct {
		URL         string `json:"url"`
		Fingerprint string `json:"secretFingerprint"`
		CreatedAt   string `json:"secretCreatedAt"`
		RotatedAt   string `json:"secretRotatedAt"`
	} `json:"webhook"`
	Credential *struct {
		Fingerprint string `json:"fingerprint"`
		CreatedAt   string `json:"createdAt"`
		RotatedAt   string `json:"rotatedAt"`
	} `json:"credential"`
	LastVerifiedAt   string                 `json:"lastVerifiedAt"`
	VerifyNote       string                 `json:"verifyNote"`
	VerifyFacts      map[string]any         `json:"verifyFacts"`
	ToolAvailability []toolAvailabilityBody `json:"toolAvailability"`
	Inbound          *struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	} `json:"inbound"`
}

// toolAvailabilityBody is one Tool as the integration read reports it: available or not,
// and why not. Served rather than joined in a console, so every client reads one answer.
type toolAvailabilityBody struct {
	Tool      string `json:"tool"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// tool finds one reported Tool by name.
func (b integrationBody) tool(t *testing.T, name string) toolAvailabilityBody {
	t.Helper()
	for _, reported := range b.ToolAvailability {
		if reported.Tool == name {
			return reported
		}
	}
	t.Fatalf("no Tool %q is reported at all; reported: %+v", name, b.ToolAvailability)
	return toolAvailabilityBody{}
}

type createdBody struct {
	Integration   integrationBody `json:"integration"`
	WebhookSecret string          `json:"webhookSecret"`
}

// createAlertmanager creates one Alertmanager Integration and returns it with the secret
// the response carried.
func (p *integrationPlane) createAlertmanager(t *testing.T, name string) createdBody {
	t.Helper()

	status, body := p.call(t, http.MethodPost, p.base(surfaceOrg)+"/integrations", map[string]any{
		"type": "alertmanager",
		"name": name,
	})
	if status != http.StatusCreated {
		t.Fatalf("creating an alertmanager integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	return created
}

// alertmanagerPayload is one firing alert in the v4 webhook shape.
func alertmanagerPayload(fingerprint, groupKey string) []byte {
	return fmt.Appendf(nil, `{
		"groupKey": %q,
		"alerts": [{
			"status": "firing",
			"fingerprint": %q,
			"labels": {"alertname": "DiskFull", "namespace": "payments"},
			"annotations": {"summary": "the disk is filling up"},
			"startsAt": %q
		}]
	}`, groupKey, fingerprint, alertmanagerAt)
}

func TestIntegrationTypeCatalog(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)

	status, body := plane.call(t, http.MethodGet, base+"/integration-types", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the catalog = %d: %s", status, body)
	}
	type catalogType struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Logo        string `json:"logo"`
		Category    string `json:"category"`
		Tools       []struct {
			Name string `json:"name"`
		} `json:"tools"`
		RequiresRelay       bool            `json:"requiresRelay"`
		ReceivesWebhooks    bool            `json:"receivesWebhooks"`
		ConfigurationSchema json.RawMessage `json:"configurationSchema"`
		Configured          int             `json:"configured"`
		// DocumentationURL is the VENDOR's page and ProductDocumentationURL is ours. Both
		// are served because an operator setting up Alertmanager needs both: Prometheus's
		// webhook_config reference, and our receiver YAML with the header name and the
		// version floor.
		DocumentationURL        string `json:"documentationUrl"`
		ProductDocumentationURL string `json:"productDocumentationUrl"`
	}
	var catalog struct {
		Types []catalogType `json:"types"`
	}
	decodeInto(t, body, &catalog)

	byKey := map[string]int{}
	for index, entry := range catalog.Types {
		byKey[entry.Key] = index
	}
	for _, wanted := range []string{"alertmanager", "kubernetes"} {
		if _, present := byKey[wanted]; !present {
			t.Fatalf("the catalog does not serve %q: %s", wanted, body)
		}
	}

	alertmanager := catalog.Types[byKey["alertmanager"]]
	if !alertmanager.ReceivesWebhooks || alertmanager.RequiresRelay {
		t.Errorf("alertmanager should receive webhooks and need no relay: %+v", alertmanager)
	}
	if len(alertmanager.Tools) != 0 {
		t.Errorf("alertmanager is an inbound alert source, not an investigation read provider: %+v",
			alertmanager.Tools)
	}

	kubernetes := catalog.Types[byKey["kubernetes"]]
	if !kubernetes.RequiresRelay || kubernetes.ReceivesWebhooks {
		t.Errorf("kubernetes should require a relay and receive no webhooks: %+v", kubernetes)
	}
	if len(kubernetes.Tools) != 3 {
		t.Errorf("kubernetes declares %d read Tools, want its three bounded Relay reads",
			len(kubernetes.Tools))
	}
	if !strings.Contains(string(kubernetes.ConfigurationSchema), "additionalProperties") {
		t.Error("the configuration schema is not the closed JSON Schema the create operation enforces")
	}

	t.Run("every type reaches our own documentation as well as the vendor's", func(t *testing.T) {
		// The operator on an Integration page needs OUR page — the receiver YAML, the
		// header name, the version floor — and every type named only the vendor's. The
		// page exists and was unreachable from the product.
		//
		// The path is derived from the definition rather than typed per provider, so this
		// asserts what the docs gate asserts from the other side: the URL a customer is
		// given resolves to a page that is really in this repository. A type added
		// without one fails here as well as in the gate.
		for _, entry := range catalog.Types {
			if entry.DocumentationURL == "" {
				t.Errorf("%s names no vendor documentation", entry.Key)
			}
			if entry.ProductDocumentationURL == "" {
				t.Errorf("%s names no OpenCluster documentation page", entry.Key)
				continue
			}
			want := "integrations/" + entry.Category + "/" + entry.Key
			if !strings.HasSuffix(entry.ProductDocumentationURL, want) {
				t.Errorf("%s documents at %q, want a page under %q",
					entry.Key, entry.ProductDocumentationURL, want)
				continue
			}
			page := filepath.Join("..", "..", "docs", filepath.FromSlash(want)+".mdx")
			if _, err := os.Stat(page); err != nil {
				t.Errorf("%s documents at %q, and %s does not exist",
					entry.Key, entry.ProductDocumentationURL, page)
			}
		}
	})

	t.Run("the seeded reference rows and the compiled catalog agree", func(t *testing.T) {
		// Reference-data drift is a test failure, not a runtime concern: the rows are
		// seeded by migration and the definitions are compiled, and this is the assertion
		// that keeps the two sets identical in both directions.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		database, err := pgx.Connect(ctx, plane.dsn)
		if err != nil {
			t.Fatalf("connecting to compare reference data: %v", err)
		}
		defer func() { _ = database.Close(ctx) }()

		rows, err := database.Query(ctx, `
			SELECT key, name, description, logo, category
			  FROM integration_type
			 ORDER BY key`)
		if err != nil {
			t.Fatalf("reading integration_type: %v", err)
		}
		defer rows.Close()
		seeded := map[string]catalogType{}
		for rows.Next() {
			var entry catalogType
			if err := rows.Scan(&entry.Key, &entry.Name, &entry.Description,
				&entry.Logo, &entry.Category); err != nil {
				t.Fatalf("scanning a type row: %v", err)
			}
			seeded[entry.Key] = entry
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading integration_type: %v", err)
		}

		if len(seeded) != len(catalog.Types) {
			t.Fatalf("the table seeds %v and the catalog serves %d types", seeded, len(catalog.Types))
		}
		for _, compiled := range catalog.Types {
			stored, present := seeded[compiled.Key]
			if !present {
				t.Errorf("the compiled catalog serves %q and integration_type does not", compiled.Key)
				continue
			}
			if stored.Name != compiled.Name || stored.Description != compiled.Description ||
				stored.Logo != compiled.Logo || stored.Category != compiled.Category {
				t.Errorf("integration_type metadata for %q = %+v, compiled catalog = %+v",
					compiled.Key, stored, compiled)
			}
		}
	})
}

func TestIntegrationLifecycle(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)

	created := plane.createAlertmanager(t, "Production Alertmanager")

	t.Run("the secret is shown once, with the webhook address beside it", func(t *testing.T) {
		if created.WebhookSecret == "" {
			t.Fatal("no webhook secret was returned; the operator has nothing to configure")
		}
		if created.Integration.Webhook == nil {
			t.Fatal("no webhook block; the operator has no address to deliver to")
		}
		if !strings.Contains(created.Integration.Webhook.URL,
			"/intake/v1/integrations/"+created.Integration.ID+"/signals") {
			t.Errorf("the webhook address %q does not name this integration",
				created.Integration.Webhook.URL)
		}
		if created.Integration.Webhook.Fingerprint == "" {
			t.Error("no secret fingerprint; an operator cannot tell one secret from the next")
		}

		status, body := plane.call(t, http.MethodGet,
			base+"/integrations/"+created.Integration.ID, nil)
		if status != http.StatusOK {
			t.Fatalf("reading the integration back = %d: %s", status, body)
		}
		if strings.Contains(body, created.WebhookSecret) {
			t.Fatal("a read returned the webhook secret; it must exist in one response only")
		}
	})

	t.Run("several integrations of one type are expressly allowed", func(t *testing.T) {
		second := plane.createAlertmanager(t, "Staging Alertmanager")
		if second.Integration.ID == created.Integration.ID {
			t.Fatal("two creates returned one integration")
		}
	})

	t.Run("a duplicate name is refused with a conflict", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type": "alertmanager",
			"name": "Production Alertmanager",
		})
		if status != http.StatusConflict {
			t.Errorf("a duplicate name = %d, want 409: %s", status, body)
		}
	})

	t.Run("an unknown type is refused in the operator's language", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type": "grafana",
			"name": "No Adapter Yet",
		})
		if status != http.StatusBadRequest {
			t.Errorf("an unserved type = %d, want 400: %s", status, body)
		}
	})

	t.Run("a kubernetes integration requires its relay", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type": "kubernetes",
			"name": "Cluster Without A Relay",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("kubernetes without a relay = %d, want 400: %s", status, body)
		}

		status, body = plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type":    "kubernetes",
			"name":    "Production Cluster",
			"relayId": plane.relay.registration.String(),
		})
		if status != http.StatusCreated {
			t.Fatalf("kubernetes with the enrolled relay = %d: %s", status, body)
		}
		var bound createdBody
		decodeInto(t, body, &bound)
		if bound.WebhookSecret != "" {
			t.Error("a kubernetes integration was handed a webhook secret it cannot use")
		}
		if bound.Integration.RelayID != plane.relay.registration.String() {
			t.Errorf("bound to relay %q, want %q",
				bound.Integration.RelayID, plane.relay.registration)
		}
	})

	t.Run("a configuration field nothing declares is refused, never dropped", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type":          "kubernetes",
			"name":          "Cluster With A Typo",
			"relayId":       plane.relay.registration.String(),
			"configuration": map[string]any{"namespaceAllowLust": "prod"},
		})
		if status != http.StatusBadRequest {
			t.Errorf("an undeclared configuration field = %d, want 400 — accepting it "+
				"silently is how a configuration comes to look complete and do nothing: %s",
				status, body)
		}
	})

	t.Run("a rename keeps identity and a listing filter finds by type", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPatch,
			base+"/integrations/"+created.Integration.ID,
			map[string]any{"name": "Primary Alertmanager"})
		if status != http.StatusOK {
			t.Fatalf("renaming = %d: %s", status, body)
		}
		var renamed integrationBody
		decodeInto(t, body, &renamed)
		if renamed.Name != "Primary Alertmanager" || renamed.ID != created.Integration.ID {
			t.Errorf("rename produced %+v", renamed)
		}

		status, body = plane.call(t, http.MethodGet, base+"/integrations?type=alertmanager", nil)
		if status != http.StatusOK {
			t.Fatalf("filtering by type = %d: %s", status, body)
		}
		var listed struct {
			Items []integrationBody `json:"items"`
		}
		decodeInto(t, body, &listed)
		for _, found := range listed.Items {
			if found.Type != "alertmanager" {
				t.Errorf("a type filter returned %q", found.Type)
			}
		}
	})

	t.Run("another tenant's view holds nothing", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet,
			plane.base(neighbourOrg)+"/integrations/"+created.Integration.ID, nil)
		if status != http.StatusNotFound {
			t.Errorf("a neighbour reading this tenant's integration = %d, want 404: %s",
				status, body)
		}
	})

	t.Run("a fresh integration deletes; one with history is refused", func(t *testing.T) {
		disposable := plane.createAlertmanager(t, "Created By Mistake")
		status, body := plane.call(t, http.MethodDelete,
			base+"/integrations/"+disposable.Integration.ID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("deleting a fresh integration = %d: %s", status, body)
		}

		delivered := plane.createAlertmanager(t, "Has Received Alerts")
		if status, body := plane.deliver(t, delivered.Integration.ID, delivered.WebhookSecret,
			alertmanagerPayload("finger-delete", "group-delete")); status != http.StatusAccepted {
			t.Fatalf("the seeding delivery = %d: %s", status, body)
		}
		status, body = plane.call(t, http.MethodDelete,
			base+"/integrations/"+delivered.Integration.ID, nil)
		if status != http.StatusConflict {
			t.Errorf("deleting an integration with signals = %d, want 409 — the record of "+
				"what a source produced must survive: %s", status, body)
		}
	})
}

func TestAlertmanagerDeliveryEndToEnd(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)
	created := plane.createAlertmanager(t, "Production Alertmanager")

	t.Run("a delivery with the minted secret is accepted", func(t *testing.T) {
		status, body := plane.deliver(t, created.Integration.ID, created.WebhookSecret,
			alertmanagerPayload("finger-1", "group-1"))
		if status != http.StatusAccepted {
			t.Fatalf("a real delivery = %d: %s", status, body)
		}
	})

	t.Run("the alert became an incident an operator can read", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, base+"/incidents", nil)
		if status != http.StatusOK {
			t.Fatalf("listing incidents = %d: %s", status, body)
		}
		var listed struct {
			Items []struct {
				ID            string `json:"id"`
				IntegrationID string `json:"integrationId"`
				Title         string `json:"title"`
				Status        string `json:"status"`
			} `json:"items"`
		}
		decodeInto(t, body, &listed)
		if len(listed.Items) == 0 {
			t.Fatalf("a delivery was accepted and no episode exists: %s", body)
		}
		episode := listed.Items[0]
		if episode.IntegrationID != created.Integration.ID {
			t.Errorf("the episode names integration %q, want %q",
				episode.IntegrationID, created.Integration.ID)
		}
		if episode.Title != "DiskFull" {
			t.Errorf("the episode is called %q, want the alert's own name", episode.Title)
		}
		if episode.Status != "open" {
			t.Errorf("a firing alert produced a %q episode", episode.Status)
		}
	})

	t.Run("a replayed body is applied to nothing", func(t *testing.T) {
		status, body := plane.deliver(t, created.Integration.ID, created.WebhookSecret,
			alertmanagerPayload("finger-1", "group-1"))
		if status != http.StatusOK {
			t.Errorf("a replay = %d, want 200 already-accepted — the source must be told "+
				"to stop and nothing may be written twice: %s", status, body)
		}
	})

	t.Run("a wrong secret is refused without learning anything", func(t *testing.T) {
		status, _ := plane.deliver(t, created.Integration.ID,
			"not-the-secret-anyone-was-given-and-long-enough",
			alertmanagerPayload("finger-2", "group-2"))
		if status != http.StatusUnauthorized {
			t.Errorf("a wrong secret = %d, want 401", status)
		}
	})

	t.Run("verification is honest about what a delivery proved", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/integrations/"+created.Integration.ID+"/verify", nil)
		if status != http.StatusOK {
			t.Fatalf("verifying = %d: %s", status, body)
		}
		var verified integrationBody
		decodeInto(t, body, &verified)
		if verified.Status != "active" {
			t.Errorf("an integration that accepted a delivery verifies as %q, want active; "+
				"note: %s", verified.Status, verified.VerifyNote)
		}
		if verified.LastVerifiedAt == "" {
			t.Error("no last-verified time; an operator cannot tell a fresh check from a stale one")
		}
	})

	t.Run("a rotation ends the old secret and starts the new one", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/integrations/"+created.Integration.ID+"/webhook/rotate-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("rotating = %d: %s", status, body)
		}
		var rotated struct {
			WebhookSecret string `json:"webhookSecret"`
			Fingerprint   string `json:"secretFingerprint"`
		}
		decodeInto(t, body, &rotated)
		if rotated.WebhookSecret == "" || rotated.WebhookSecret == created.WebhookSecret {
			t.Fatal("a rotation returned no new secret")
		}

		if status, _ := plane.deliver(t, created.Integration.ID, created.WebhookSecret,
			alertmanagerPayload("finger-3", "group-3")); status != http.StatusUnauthorized {
			t.Errorf("the previous secret still works after a rotation = %d, want 401", status)
		}
		if status, body := plane.deliver(t, created.Integration.ID, rotated.WebhookSecret,
			alertmanagerPayload("finger-3", "group-3")); status != http.StatusAccepted {
			t.Errorf("the rotated secret = %d, want 202: %s", status, body)
		}
	})

	t.Run("a disabled integration refuses deliveries", func(t *testing.T) {
		muted := plane.createAlertmanager(t, "Muted Alertmanager")
		status, body := plane.call(t, http.MethodPost,
			base+"/integrations/"+muted.Integration.ID+"/enabled",
			map[string]any{"enabled": false})
		if status != http.StatusNoContent {
			t.Fatalf("disabling = %d: %s", status, body)
		}
		if status, _ := plane.deliver(t, muted.Integration.ID, muted.WebhookSecret,
			alertmanagerPayload("finger-4", "group-4")); status != http.StatusUnauthorized {
			t.Errorf("a delivery to a disabled integration = %d, want 401 — an operator "+
				"who turned it off wants deliveries refused, not merely recorded", status)
		}
	})
}

// The two packages that decide "connected" cannot import each other, so the agreement is
// asserted here: a verification and the fleet roster must never disagree about whether one
// relay is alive.
func TestLivenessAllowancesAgree(t *testing.T) {
	t.Parallel()
	if storage.LivenessAllowance != relay.LivenessAllowance {
		t.Fatalf("storage answers connected within %s and the session protocol within %s; "+
			"two definitions of alive is one of them being wrong",
			storage.LivenessAllowance, relay.LivenessAllowance)
	}
}

func TestKubernetesVerification(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)

	status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
		"type":    "kubernetes",
		"name":    "Production Cluster",
		"relayId": plane.relay.registration.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("creating the kubernetes integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	// The harness enrols the relay and opens no session, so the honest verification is a
	// failure that says the relay is not connected — never an "active" resting on a form
	// having validated.
	status, body = plane.call(t, http.MethodPost,
		base+"/integrations/"+created.Integration.ID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying = %d: %s", status, body)
	}
	var verified integrationBody
	decodeInto(t, body, &verified)
	if verified.Status != "failed" {
		t.Errorf("a cluster whose relay never connected verifies as %q, want failed; note: %s",
			verified.Status, verified.VerifyNote)
	}
	if !strings.Contains(verified.VerifyNote, "not connected") {
		t.Errorf("the note %q does not say what is wrong in the operator's language",
			verified.VerifyNote)
	}
}

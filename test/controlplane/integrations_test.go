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
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

const (
	surfaceOrg     = "11111111-1111-4111-8111-111111111111"
	neighbourOrg   = "22222222-2222-4222-8222-222222222222"
	alertmanagerAt = "2026-01-02T15:04:05Z"
)

type integrationPlane struct {
	*controlPlane
	api     string
	intake  string
	relayAt string
	relay   relayCredentials
	dsn     string
}

func startIntegrationPlane(t *testing.T) *integrationPlane {
	t.Helper()
	return startIntegrationPlaneWithOptions(t, app.Options{})
}

func startIntegrationPlaneWithOptions(t *testing.T, options app.Options) *integrationPlane {
	t.Helper()

	apiAddress := freeAddress(t)
	relayAddress := freeAddress(t)
	intakeAddress := apiAddress
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		cfg.HTTPListenAddress = intakeAddress
		cfg.RelayListenAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		dsn = cfg.DatabaseDSN
	}, options)

	connection := dialRelay(t, relayAddress)
	return &integrationPlane{
		controlPlane: plane,
		api:          apiAddress,
		intake:       intakeAddress,
		relayAt:      relayAddress,
		relay:        registerRelay(t, connection, dsn, surfaceOrg),
		dsn:          dsn,
	}
}

func (p *integrationPlane) base(organization string) string {
	return "http://" + p.api + "/api/v1"
}

func (p *integrationPlane) call(
	t *testing.T, method, url string, body any, headers ...http.Header,
) (int, string) {
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
	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: p.sessionCookie})
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("Origin", "http://"+p.api)
	}
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, supplied := range headers {
		for name, values := range supplied {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
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

func (p *integrationPlane) deliver(
	t *testing.T, integrationID, secret string, payload []byte,
) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	url := "http://" + p.intake + "/webhooks/v1/integrations/" + integrationID + "/alert-events"
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
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Disconnected  bool           `json:"disconnected"`
	Configuration map[string]any `json:"configuration"`
	RelayID       string         `json:"relayId"`
	Webhook       *struct {
		URL        string `json:"url"`
		Configured bool   `json:"configured"`
	} `json:"webhook"`
	Credential *struct {
		Configured bool `json:"configured"`
	} `json:"credential"`
	VerifiedAt       string                 `json:"verifiedAt"`
	VerificationNote string                 `json:"verificationNote"`
	ToolAvailability []toolAvailabilityBody `json:"toolAvailability"`
}

type toolAvailabilityBody struct {
	Tool      string `json:"tool"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

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
		t.Fatalf("catalog = %d: %s", status, body)
	}
	var catalog map[string]json.RawMessage
	decodeInto(t, body, &catalog)
	if len(catalog) != 1 {
		t.Fatalf("catalog must contain only types: %s", body)
	}
	var integrationTypes []map[string]json.RawMessage
	decodeInto(t, string(catalog["types"]), &integrationTypes)
	keys := []string{"alertmanager", "generic_webhook", "github", "kubernetes", "loki", "prometheus", "slack", "tempo"}
	if len(integrationTypes) != len(keys) {
		t.Fatalf("catalog count = %d", len(integrationTypes))
	}
	for index, integrationType := range integrationTypes {
		if len(integrationType) != 4 {
			t.Fatalf("catalog card has extra fields: %v", integrationType)
		}
		for _, field := range []string{"key", "name", "description", "category"} {
			if integrationType[field] == nil {
				t.Fatalf("card is missing %s", field)
			}
		}
		var key string
		decodeInto(t, string(integrationType["key"]), &key)
		if key != keys[index] {
			t.Fatalf("catalog key = %q, want %q", key, keys[index])
		}
	}
	for _, key := range []string{"loki", "slack", "github", "kubernetes", "alertmanager"} {
		status, body := plane.call(t, http.MethodGet, base+"/integration-types/"+key, nil)
		if status != http.StatusOK {
			t.Fatalf("type detail %s = %d: %s", key, status, body)
		}
		var detail struct {
			Key                 string                       `json:"key"`
			RequiresRelay       bool                         `json:"requiresRelay"`
			SupportsConnect     bool                         `json:"supportsConnect"`
			ConfigurationFields []map[string]json.RawMessage `json:"configurationFields"`
		}
		decodeInto(t, body, &detail)
		if detail.Key != key || detail.RequiresRelay != (key == "kubernetes") || detail.SupportsConnect {
			t.Fatalf("unexpected type detail: %s", body)
		}
		var rawDetail map[string]json.RawMessage
		decodeInto(t, body, &rawDetail)
		if len(rawDetail) != 7 || detail.ConfigurationFields == nil {
			t.Fatalf("unexpected detail shape: %s", body)
		}
		for _, field := range detail.ConfigurationFields {
			for _, property := range []string{"key", "type", "required", "secret"} {
				if field[property] == nil {
					t.Fatalf("configuration field missing %s: %v", property, field)
				}
			}
			if len(field) > 5 {
				t.Fatalf("unsafe configuration projection: %v", field)
			}
		}
		if key == "slack" && (len(detail.ConfigurationFields) != 1 || string(detail.ConfigurationFields[0]["key"]) != "\"botToken\"" || string(detail.ConfigurationFields[0]["secret"]) != "true" || string(detail.ConfigurationFields[0]["required"]) != "true") {
			t.Fatalf("manual Slack token metadata = %s", body)
		}
		if key == "loki" {
			for _, field := range detail.ConfigurationFields {
				if string(field["key"]) == "\"auth\"" && string(field["options"]) != `["none","bearer","basic"]` {
					t.Fatalf("Loki authentication metadata = %s", body)
				}
				if string(field["key"]) == "\"url\"" && (string(field["required"]) != "true" || string(field["secret"]) != "false") {
					t.Fatalf("Loki URL metadata = %s", body)
				}
			}
		}
	}
	status, body = plane.call(t, http.MethodGet, base+"/integration-types/unknown", nil)
	var refusal map[string]string
	decodeInto(t, body, &refusal)
	if status != http.StatusNotFound || len(refusal) != 1 || refusal["error"] == "" {
		t.Fatalf("unknown type = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, base+"/integrations", map[string]any{"type": "github", "name": "Unauthenticated installation"})
	if status != http.StatusBadRequest {
		t.Fatalf("manual GitHub creation = %d: %s", status, body)
	}
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
			"/webhooks/v1/integrations/"+created.Integration.ID+"/alert-events") {
			t.Errorf("the webhook address %q does not name this integration",
				created.Integration.Webhook.URL)
		}
		if !created.Integration.Webhook.Configured {
			t.Error("the response does not report the webhook token as configured")
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

	t.Run("duplicate display names remain addressable by id", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type": "alertmanager",
			"name": "Production Alertmanager",
		})
		if status != http.StatusCreated {
			t.Fatalf("a duplicate display name = %d: %s", status, body)
		}
		var duplicate createdBody
		decodeInto(t, body, &duplicate)
		if duplicate.Integration.ID == created.Integration.ID {
			t.Fatal("duplicate display names collapsed to one integration")
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
		defer plane.switchOrganization(t, neighbourOrg)()
		status, body := plane.call(t, http.MethodGet,
			plane.base(neighbourOrg)+"/integrations/"+created.Integration.ID, nil)
		if status != http.StatusNotFound {
			t.Errorf("a neighbour reading this tenant's integration = %d, want 404: %s",
				status, body)
		}
	})

	t.Run("disconnect preserves history and refuses future intake", func(t *testing.T) {
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
		if status != http.StatusNoContent {
			t.Fatalf("disconnecting an integration with history = %d, want 204: %s", status, body)
		}
		status, body = plane.call(t, http.MethodDelete,
			base+"/integrations/"+delivered.Integration.ID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("repeated disconnect = %d: %s", status, body)
		}
		status, body = plane.call(t, http.MethodGet, base+"/integrations/"+delivered.Integration.ID, nil)
		var retained struct {
			Disconnected bool `json:"disconnected"`
		}
		decodeInto(t, body, &retained)
		if status != http.StatusOK || !retained.Disconnected {
			t.Fatalf("retained disconnected integration = %d: %s", status, body)
		}
		if status, _ := plane.call(t, http.MethodPost, base+"/integrations/"+delivered.Integration.ID+"/verify", nil); status != http.StatusConflict {
			t.Fatalf("disconnected verification = %d, want 409", status)
		}
		if status, _ := plane.call(t, http.MethodPatch, base+"/integrations/"+delivered.Integration.ID, map[string]any{"name": "revive"}); status != http.StatusConflict {
			t.Fatalf("disconnected revision = %d, want 409", status)
		}
		if status, _ := plane.deliver(t, delivered.Integration.ID, delivered.WebhookSecret,
			alertmanagerPayload("after-disconnect", "after-disconnect")); status != http.StatusUnauthorized {
			t.Fatalf("delivery after disconnect = %d, want 401", status)
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
			t.Fatalf("a delivery was accepted and no incident exists: %s", body)
		}
		incident := listed.Items[0]
		if incident.IntegrationID != created.Integration.ID {
			t.Errorf("the incident names integration %q, want %q",
				incident.IntegrationID, created.Integration.ID)
		}
		if incident.Title != "DiskFull" {
			t.Errorf("the incident is called %q, want the alert's own name", incident.Title)
		}
		if incident.Status != "open" {
			t.Errorf("a firing alert produced a %q incident", incident.Status)
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
		if verified.Status != "verified" {
			t.Errorf("an integration that accepted a delivery verifies as %q, want verified; "+
				"note: %s", verified.Status, verified.VerificationNote)
		}
		if verified.VerifiedAt == "" {
			t.Error("no last-verified time; an operator cannot tell a fresh check from a stale one")
		}
	})

	t.Run("a rotation ends the old secret and starts the new one", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/integrations/"+created.Integration.ID+"/rotate-webhook-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("rotating = %d: %s", status, body)
		}
		var rotated struct {
			WebhookSecret string `json:"webhookSecret"`
			Effect        string `json:"effect"`
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

	t.Run("a disconnected integration refuses deliveries", func(t *testing.T) {
		muted := plane.createAlertmanager(t, "Muted Alertmanager")
		status, body := plane.call(t, http.MethodDelete,
			base+"/integrations/"+muted.Integration.ID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("disabling = %d: %s", status, body)
		}
		if status, _ := plane.deliver(t, muted.Integration.ID, muted.WebhookSecret,
			alertmanagerPayload("finger-4", "group-4")); status != http.StatusUnauthorized {
			t.Errorf("a delivery to a disconnected integration = %d, want 401 — an operator "+
				"who turned it off wants deliveries refused, not merely recorded", status)
		}
	})
}

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

	status, body = plane.call(t, http.MethodPost,
		base+"/integrations/"+created.Integration.ID+"/verify", nil)
	if status != http.StatusOK {
		t.Fatalf("verifying = %d: %s", status, body)
	}
	var verified integrationBody
	decodeInto(t, body, &verified)
	if verified.Status != "failed" {
		t.Errorf("a cluster whose relay never connected verifies as %q, want failed; note: %s",
			verified.Status, verified.VerificationNote)
	}
	if !strings.Contains(verified.VerificationNote, "not connected") {
		t.Errorf("the note %q does not say what is wrong in the operator's language",
			verified.VerificationNote)
	}
}

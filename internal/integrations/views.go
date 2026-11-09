package integrations

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type errorView struct {
	Error string `json:"error"`
}

type integrationTypeView struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

type configurationFieldView struct {
	Key      string   `json:"key"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Secret   bool     `json:"secret"`
	Options  []string `json:"options,omitempty"`
}

type integrationTypeDetailView struct {
	integrationTypeView
	RequiresRelay       bool                     `json:"requiresRelay"`
	SupportsConnect     bool                     `json:"supportsConnect"`
	ConfigurationFields []configurationFieldView `json:"configurationFields"`
}

type integrationTypeListView struct {
	Types []integrationTypeView `json:"types"`
}

func integrationTypeViewOf(definition Definition) integrationTypeView {
	return integrationTypeView{Key: string(definition.Key), Name: definition.Name,
		Description: definition.Description, Category: string(definition.Category)}
}

type webhookView struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

type credentialView struct {
	Configured bool `json:"configured"`
}

type integrationView struct {
	ID               string                 `json:"id"`
	Type             string                 `json:"type"`
	Name             string                 `json:"name"`
	Status           *string                `json:"status"`
	Disconnected     bool                   `json:"disconnected"`
	Configuration    map[string]any         `json:"configuration"`
	RelayID          string                 `json:"relayId,omitempty"`
	Webhook          *webhookView           `json:"webhook,omitempty"`
	Credential       *credentialView        `json:"credential,omitempty"`
	VerifiedAt       string                 `json:"verifiedAt,omitempty"`
	VerificationNote string                 `json:"verificationNote,omitempty"`
	ToolAvailability []toolAvailabilityView `json:"toolAvailability"`
	CreatedAt        string                 `json:"createdAt"`
}

type toolAvailabilityView struct {
	Tool      string `json:"tool"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type createdView struct {
	IntegrationView integrationView `json:"integration"`
	WebhookSecret   string          `json:"webhookSecret,omitempty"`
}

type rotatedView struct {
	WebhookSecret string `json:"webhookSecret"`
	Effect        string `json:"effect"`
}

func (h Handlers) viewOf(integration Integration) integrationView {
	typeKey := ""
	availability := []toolAvailabilityView{}
	if definition, known := h.Catalog.Lookup(integration.Provider); known {
		typeKey = string(definition.Key)
		for _, toolAvailability := range ToolAvailabilityFor(definition, integration) {
			availability = append(availability, toolAvailabilityView(toolAvailability))
		}
	}
	view := integrationView{
		ID:               integration.ID.String(),
		Type:             typeKey,
		Name:             integration.Name,
		Disconnected:     integration.Disconnected,
		Configuration:    integration.Configuration,
		CreatedAt:        stamp(integration.CreatedAt),
		ToolAvailability: availability,
	}
	if status := integration.Status.String(); status != "" {
		view.Status = &status
	}
	if integration.RelayID != uuid.Nil {
		view.RelayID = integration.RelayID.String()
	}
	if !integration.VerifiedAt.IsZero() {
		view.VerifiedAt = stamp(integration.VerifiedAt)
	}
	if len(integration.WebhookSecretDigest) > 0 {
		view.Webhook = &webhookView{URL: h.webhookURL(integration.ID), Configured: true}
	}
	if len(integration.CredentialSealed) > 0 {
		view.Credential = &credentialView{Configured: true}
	}
	return view
}

func (h Handlers) webhookURL(id uuid.UUID) string {
	return h.PublicURL + "/webhooks/v1/integrations/" + id.String() + "/alert-events"
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}

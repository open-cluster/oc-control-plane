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

type typeView struct {
	Key                     string          `json:"key"`
	Name                    string          `json:"name"`
	Description             string          `json:"description"`
	Logo                    string          `json:"logo,omitempty"`
	Category                string          `json:"category"`
	Available               bool            `json:"available"`
	Capabilities            []string        `json:"capabilities"`
	SecretFields            []string        `json:"secretFields"`
	DocumentationSlug       string          `json:"documentationSlug"`
	DocumentationURL        string          `json:"documentationUrl,omitempty"`
	ProductDocumentationURL string          `json:"productDocumentationUrl,omitempty"`
	RequiresRelay           bool            `json:"requiresRelay"`
	ReceivesWebhooks        bool            `json:"receivesWebhooks"`
	SupportsConnect         bool            `json:"supportsConnect"`
	ConfigurationSchema     json.RawMessage `json:"configurationSchema"`
	Tools                   []toolView      `json:"tools,omitempty"`
	Configured              int             `json:"configured"`
}

type toolView struct {
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	WhenToUse    string             `json:"whenToUse"`
	WhenNotToUse string             `json:"whenNotToUse"`
	Arguments    []toolArgumentView `json:"arguments,omitempty"`
	Permissions  string             `json:"permissions"`
	Output       string             `json:"output"`
}

type toolArgumentView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
}

type typeListView struct {
	Types []typeView `json:"types"`
	Next  *string    `json:"next"`
}

func typeViewOf(definition Definition, configured int, receivesWebhooks bool) typeView {
	manifest := definition.Manifest
	tools := make([]toolView, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		arguments := make([]toolArgumentView, 0, len(tool.Arguments))
		for _, argument := range tool.Arguments {
			arguments = append(arguments, toolArgumentView{
				Name:        argument.Name,
				Description: argument.Description,
				Type:        string(argument.Type),
				Required:    argument.Required,
			})
		}
		tools = append(tools, toolView{
			Name:         tool.Name,
			Description:  tool.Description,
			WhenToUse:    tool.WhenToUse,
			WhenNotToUse: tool.WhenNotToUse,
			Arguments:    arguments,
			Permissions:  tool.Permissions,
			Output:       tool.Output,
		})
	}
	return typeView{
		Key:                     string(manifest.Key),
		Name:                    manifest.Name,
		Description:             manifest.Description,
		Logo:                    manifest.Logo,
		Category:                string(manifest.Category),
		Available:               true,
		Capabilities:            manifest.Capabilities(),
		SecretFields:            manifest.SecretFields(),
		DocumentationSlug:       manifest.DocumentationSlug,
		DocumentationURL:        manifest.SourceURL,
		ProductDocumentationURL: manifest.ProductDocumentationURL(),
		RequiresRelay:           manifest.RequiresRelay,
		ReceivesWebhooks:        receivesWebhooks,
		SupportsConnect:         definition.Connectable(),
		ConfigurationSchema:     manifest.ConfigurationSchema(),
		Tools:                   tools,
		Configured:              configured,
	}
}

type webhookView struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

type credentialView struct {
	Configured bool `json:"configured"`
}

type integrationView struct {
	ID               string                   `json:"id"`
	Type             string                   `json:"type"`
	Name             string                   `json:"name"`
	Status           *string                  `json:"status"`
	Disabled         bool                     `json:"disabled"`
	Configuration    map[string]any           `json:"configuration"`
	RelayID          string                   `json:"relayId,omitempty"`
	Webhook          *webhookView             `json:"webhook,omitempty"`
	Credential       *credentialView          `json:"credential,omitempty"`
	VerifiedAt       string                   `json:"verifiedAt,omitempty"`
	VerificationNote string                   `json:"verificationNote,omitempty"`
	Inbound          *inboundAvailabilityView `json:"inbound,omitempty"`
	ToolAvailability []toolAvailabilityView   `json:"toolAvailability"`
	CreatedAt        string                   `json:"createdAt"`
}

type toolAvailabilityView struct {
	Tool      string `json:"tool"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type inboundAvailabilityView struct {
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

func (h Handlers) viewOf(found Integration) integrationView {
	typeKey := ""
	availability := []toolAvailabilityView{}
	var inbound *inboundAvailabilityView
	if definition, known := h.Catalog.Lookup(found.Provider); known {
		typeKey = string(definition.Key)
		if definition.Inbound != nil {
			reported := definition.Inbound(found)
			inbound = &inboundAvailabilityView{
				Available: reported.Available, Reason: reported.Reason,
			}
		}
		for _, one := range ToolAvailabilityFor(definition, found) {
			availability = append(availability, toolAvailabilityView(one))
		}
	}
	view := integrationView{
		ID:               found.ID.String(),
		Type:             typeKey,
		Name:             found.Name,
		Disabled:         found.Disabled,
		Configuration:    found.Configuration,
		CreatedAt:        stamp(found.CreatedAt),
		Inbound:          inbound,
		ToolAvailability: availability,
	}
	if status := found.Status.String(); status != "" {
		view.Status = &status
	}
	if found.RelayID != uuid.Nil {
		view.RelayID = found.RelayID.String()
	}
	if !found.VerifiedAt.IsZero() {
		view.VerifiedAt = stamp(found.VerifiedAt)
	}
	if len(found.WebhookSecretDigest) > 0 {
		view.Webhook = &webhookView{URL: h.webhookURL(found.ID), Configured: true}
	}
	if len(found.CredentialSealed) > 0 {
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

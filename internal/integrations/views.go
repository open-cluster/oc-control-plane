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

// typeView is one Integration Type as the catalog renders it.
type typeView struct {
	Key               string   `json:"key"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Logo              string   `json:"logo,omitempty"`
	Category          string   `json:"category"`
	Available         bool     `json:"available"`
	Capabilities      []string `json:"capabilities"`
	SecretFields      []string `json:"secretFields"`
	DocumentationSlug string   `json:"documentationSlug"`
	// DocumentationURL points at the provider's own setup documentation.
	DocumentationURL string `json:"documentationUrl,omitempty"`
	// ProductDocumentationURL points at OURS: the page that carries the receiver
	// configuration, the header this deployment expects and the version floor. Served
	// beside the vendor's rather than instead of it, because they answer different
	// questions and a caller labels them differently.
	ProductDocumentationURL string `json:"productDocumentationUrl,omitempty"`
	RequiresRelay           bool   `json:"requiresRelay"`
	ReceivesWebhooks        bool   `json:"receivesWebhooks"`
	// SupportsConnect says this deployment can connect the type through the provider's
	// own installation flow, so a setup surface offers one button instead of a form.
	// False is the self-hosted deployment that registered no application with the
	// vendor, and the configuration form is what it renders.
	SupportsConnect bool `json:"supportsConnect"`
	// ConfigurationSchema is JSON Schema for the type's settings, rendered from the
	// definition so it cannot drift from what the create operation accepts.
	ConfigurationSchema json.RawMessage `json:"configurationSchema"`
	// Tools is what connecting this type lets an investigation read, with the routing
	// guidance each tool declares. Rendered so a setup flow can say what connecting DOES.
	Tools []toolView `json:"tools,omitempty"`
	// Configured is how many Integrations of this type the tenant has.
	Configured int `json:"configured"`
}

// toolView is one declared tool as the catalog renders it.
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

// webhookView is what a read says about an inbound endpoint. It carries the identity of
// the live secret and where deliveries go — never the secret.
type webhookView struct {
	// URL is where the source delivers, when this deployment has been told its public
	// intake origin; the path alone otherwise.
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

// credentialView is what a read says about the outbound credential: its minted identity
// and its lifecycle — never the credential and never the sealed bytes.
type credentialView struct {
	Configured bool `json:"configured"`
}

// integrationView is what an Integration looks like to an operator. It carries no secret
// and no digest: publishing the digest would let anyone holding a database dump confirm a
// guess offline, which is exactly the property digest-only storage exists for.
type integrationView struct {
	ID     string  `json:"id"`
	Type   string  `json:"type"`
	Name   string  `json:"name"`
	Status *string `json:"status"`
	// Disabled is the operator's own switch, orthogonal to status.
	Disabled         bool            `json:"disabled"`
	Configuration    map[string]any  `json:"configuration"`
	RelayID          string          `json:"relayId,omitempty"`
	Webhook          *webhookView    `json:"webhook,omitempty"`
	Credential       *credentialView `json:"credential,omitempty"`
	VerifiedAt       string          `json:"verifiedAt,omitempty"`
	VerificationNote string          `json:"verificationNote,omitempty"`
	// Inbound reports provider-specific interaction setup separately from investigation
	// Tools; a credential can support reads without supporting inbound app mentions.
	Inbound *inboundAvailabilityView `json:"inbound,omitempty"`
	// ToolAvailability reports every Tool this Integration Type declares, judged against
	// the grants its last Verification recorded.
	//
	// Served rather than left to be joined by a caller. The same rule decides which tools
	// an investigation is offered, so a second copy of it would be free to disagree; and
	// the join needs the type's declarations, the integration's grants and — for some
	// providers — deployment configuration a browser cannot see.
	ToolAvailability []toolAvailabilityView `json:"toolAvailability"`
	CreatedAt        string                 `json:"createdAt"`
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

// createdView is the one response that carries the webhook secret, exactly once.
type createdView struct {
	IntegrationView integrationView `json:"integration"`
	WebhookSecret   string          `json:"webhookSecret,omitempty"`
}

// rotatedView answers a rotation: the new secret, once, and what the rotation cost.
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
			// A conversion rather than a field-by-field copy, and it is the stricter of
			// the two: it compiles only while the two shapes are identical, so a field
			// added to the domain type stops the build here until somebody decides what
			// the wire should say about it. A copy would have silently said nothing.
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

// webhookURL is where a source delivers to this Integration. The path is served even when
// the deployment has not said where intake is publicly reachable, because the path is true
// either way and an operator joining it to an origin they know beats a guess made here.
func (h Handlers) webhookURL(id uuid.UUID) string {
	return h.IntakeBaseURL + "/webhooks/v1/integrations/" + id.String() + "/alert-events"
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// writeJSON answers with a body. Nothing this surface returns may be cached: every answer
// concerns a named tenant's record.
func writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}

package connection

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What this surface says on the wire. Kept apart from the handlers because it is a contract:
// a field renamed here is a client broken somewhere else.

const maxRequestBytes = 16 << 10

type listView struct {
	Connections []connectionView `json:"connections"`
	Next        string           `json:"next,omitempty"`
}

// connectionView is what a Connection looks like to an operator. It carries no secret and no
// digest: the digest is not a credential but publishing it would let anyone holding a database
// dump confirm a guess offline, which is exactly the property digest-only storage exists for.
type connectionView struct {
	ID          string `json:"id"`
	Environment string `json:"environmentId"`
	// Integration is the KIND of system this is an instance of, and Name is which instance.
	// Both are shown because an operator with two Alertmanager Connections needs to see that
	// they share an integration and differ in everything else.
	Integration string `json:"integration"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Locality    string `json:"locality"`
	// RelayRegistrationID is absent for a control-plane connection, which no Relay serves.
	RelayRegistrationID string            `json:"relayRegistrationId,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	// DisabledAt is absent while the connection is live, so its presence is the state rather
	// than a field an operator has to compare against a zero value.
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// createdView is the one response that carries a secret, and the only time one exists in a
// readable form anywhere in this system.
type createdView struct {
	Connection   connectionView `json:"connection"`
	Secret       string         `json:"secret,omitempty"`
	SecretNotice string         `json:"secretNotice,omitempty"`
}

type rotatedView struct {
	Secret       string `json:"secret"`
	SecretNotice string `json:"secretNotice"`
}

type createRequest struct {
	// EnvironmentID is the scope this Connection belongs to. It moved out of the path when the
	// collection became Organization-wide; it is still assigned exactly once, here, and the
	// composite foreign key still refuses one belonging to another tenant.
	EnvironmentID string `json:"environmentId"`
	Integration   string `json:"integration"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Locality      string `json:"locality"`
	// RelayRegistrationID names the installation that serves a relay-local connection, and is
	// refused on a central one rather than ignored.
	RelayRegistrationID string `json:"relayRegistrationId"`
	// Secret is optional. Omitted, the platform mints one; supplied, it must clear the same
	// floor a minted one clears by construction.
	Secret string            `json:"secret"`
	Labels map[string]string `json:"labels"`
	// Configuration is the provider-specific settings, shaped by the Integration definition's
	// JSON Schema. A field the definition does not declare is refused rather than ignored: the
	// schema is closed, and a caller who misspelled one should be told rather than left
	// believing they configured something.
	Configuration map[string]any `json:"configuration"`
}

type secretRequest struct {
	Secret string `json:"secret"`
}

// integrationView is one catalog entry: everything a schema-driven setup flow needs to render
// a provider, and everything an operator needs to decide whether to click it.
//
// The field doing the most work is Lifecycle. `general` and `preview` are actionable; `planned`
// and `deprecated` are labelled and NOT — and a client that renders a planned provider as
// clickable is offering an afternoon of configuring something that cannot work. The backend
// refuses it either way, but the refusal arrives after the form has been filled in.
type integrationView struct {
	// ID and Integration are the same string: the value a connection row stores. Both are here
	// so a client may key on one and send the other, and they coincide because the persisted
	// value is already URL-safe.
	ID          string `json:"id"`
	Integration string `json:"integration"`
	Slug        string `json:"slug"`
	// DefinitionVersion lets a client built against an older definition detect that it is stale
	// rather than mis-render it.
	DefinitionVersion int    `json:"definitionVersion"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Category          string `json:"category"`
	// LogoAssetKey names an approved mark in the console's brand registry and carries no image.
	// It is absent far more often than present: where no approved asset exists the neutral
	// category icon is correct, and a fabricated key would render a broken image instead.
	LogoAssetKey     string   `json:"logoAssetKey,omitempty"`
	DocumentationURL string   `json:"documentationUrl"`
	AuthModes        []string `json:"authModes"`
	// Roles is the specification's `connectionModes` under the vocabulary's own word for it:
	// CONTEXT.md names this exact set the Connection ROLE and expressly forbids "mode" as a
	// synonym. Same set, correct noun.
	Roles               []string `json:"roles"`
	ExecutionLocalities []string `json:"executionLocalities"`
	// ConfigurationSchema is JSON Schema draft 2020-12, rendered from the definition's fields
	// so it cannot be invalid and cannot describe a field nothing reads.
	ConfigurationSchema       json.RawMessage        `json:"configurationSchema"`
	PresentationSchema        presentationView       `json:"presentationSchema"`
	ValidationContract        validationContractView `json:"validationContract"`
	Capabilities              []string               `json:"capabilities"`
	MinimumRelayVersion       string                 `json:"minimumRelayVersion,omitempty"`
	SupportsMultipleInstances bool                   `json:"supportsMultipleInstances"`
	Lifecycle                 string                 `json:"lifecycle"`
	// Actionable is Lifecycle's consequence stated rather than left to be derived. A client that
	// worked it out for itself would be a second copy of the rule, and the second copy is the
	// one that goes stale when a fifth lifecycle state arrives.
	Actionable bool `json:"actionable"`
	// ConfiguredConnections is how many Connections of this Integration the tenant has. It is
	// the reason this route is Organization-scoped: a catalog nobody can count against is a
	// list of what the product supports rather than a view of what the customer runs, and it is
	// what turns a tile from "Add connection" into "Configure".
	ConfiguredConnections int `json:"configuredConnections"`
}

// presentationView is how to lay the configuration out, and where a generic form gives up.
type presentationView struct {
	Sections []sectionView `json:"sections"`
	// ProviderStep is an identifier the console resolves to a purpose-built component, for the
	// cases a generic form engine cannot serve — AWS role assumption, Kubernetes service-account
	// binding. Absent for every provider an ordinary form covers, which is most of them.
	ProviderStep string `json:"providerStep,omitempty"`
}

type sectionView struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Fields      []string `json:"fields"`
}

// validationContractView says what validating this provider can establish BEFORE anybody runs
// one, so "connected" is a claim an operator can size up rather than take on trust.
type validationContractView struct {
	Authenticates      bool   `json:"authenticates"`
	ProbesCapabilities bool   `json:"probesCapabilities"`
	Note               string `json:"note"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(found storage.Connection) connectionView {
	view := connectionView{
		ID:          found.ID.String(),
		Environment: found.Environment.String(),
		Integration: found.Integration,
		Name:        found.Name,
		Role:        found.Role.String(),
		Locality:    found.Locality.String(),
		Labels:      found.Labels,
		CreatedAt:   found.CreatedAt,
		UpdatedAt:   found.UpdatedAt,
	}
	if found.RelayRegistration != uuid.Nil {
		view.RelayRegistrationID = found.RelayRegistration.String()
	}
	if found.Disabled() {
		disabled := found.DisabledAt
		view.DisabledAt = &disabled
	}
	return view
}

// roleNames renders a role set for the integrations listing.
func roleNames(role storage.ConnectionRole) []string {
	names := make([]string, 0, 2)
	if role.Includes(storage.RoleTrigger) {
		names = append(names, storage.RoleTrigger.String())
	}
	if role.Includes(storage.RoleEvidence) {
		names = append(names, storage.RoleEvidence.String())
	}
	return names
}

// catalogViewOf renders one definition together with how many Connections of it this tenant
// already has.
func catalogViewOf(definition Definition, configured int) integrationView {
	authModes := make([]string, 0, len(definition.AuthModes))
	for _, mode := range definition.AuthModes {
		authModes = append(authModes, string(mode))
	}
	localities := make([]string, 0, len(definition.ExecutionLocalities))
	for _, locality := range definition.ExecutionLocalities {
		localities = append(localities, locality.String())
	}
	sections := make([]sectionView, 0, len(definition.Presentation.Sections))
	for _, section := range definition.Presentation.Sections {
		sections = append(sections, sectionView{
			Title:       section.Title,
			Description: section.Description,
			Fields:      section.Fields,
		})
	}
	capabilities := definition.Capabilities
	if capabilities == nil {
		// An empty list rather than null. "This provider makes no typed reads available" is a
		// fact worth rendering, and a client should not have to handle two spellings of it.
		capabilities = []string{}
	}

	return integrationView{
		ID:                  definition.Slug,
		Integration:         definition.Slug,
		Slug:                definition.Slug,
		DefinitionVersion:   definition.DefinitionVersion,
		Name:                definition.Name,
		Description:         definition.Description,
		Category:            string(definition.Category),
		LogoAssetKey:        definition.LogoAssetKey,
		DocumentationURL:    definition.DocumentationURL,
		AuthModes:           authModes,
		Roles:               roleNames(definition.Roles),
		ExecutionLocalities: localities,
		ConfigurationSchema: definition.ConfigurationSchema(),
		PresentationSchema:  presentationView{Sections: sections, ProviderStep: definition.Presentation.ProviderStep},
		ValidationContract: validationContractView{
			Authenticates:      definition.Validation.Authenticates,
			ProbesCapabilities: definition.Validation.ProbesCapabilities,
			Note:               definition.Validation.Note,
		},
		Capabilities:              capabilities,
		MinimumRelayVersion:       definition.MinimumRelayVersion,
		SupportsMultipleInstances: definition.SupportsMultipleInstances,
		Lifecycle:                 string(definition.Lifecycle),
		Actionable:                definition.Lifecycle.Actionable(),
		ConfiguredConnections:     configured,
	}
}

// decode reads a bounded request body, refusing a field this build does not know rather than
// ignoring it: an operator who misspelled one should be told, not left believing they
// configured something.
func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	return true
}

// writeJSON sends a response body. Nothing this surface returns may be stored or re-typed by
// anything in front of it: one response carries a newly minted credential, and every other
// carries a named tenant's configuration.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

// enabledRequest is the body of the one operation that replaced the enable and disable pair.
//
// The field is a POINTER so that "I did not say" is distinguishable from "I said false". A
// missing body defaulting to one direction would be the API deciding, during an incident, what
// an operator meant about a Connection an investigation may be reading through.
type enabledRequest struct {
	Enabled *bool `json:"enabled"`
}

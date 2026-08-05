package connection

import (
	"encoding/json"
	"sort"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What an Integration IS, as a record rather than as a name and a role bitmask.
//
// The vocabulary stays a closed set compiled into the binary — that decision is integration.go's
// and this file does not reopen it. What changes is how much each entry carries. A catalog that
// says only "alertmanager, kubernetes" cannot tell an operator what a provider is for, whether
// it works today, whether installing anything is required, what evidence connecting it makes
// available, or what a setup form should ask for. Every one of those is a fact the product knows
// and was throwing away.
//
// The field that does the most work is Lifecycle. It is what lets the catalog be HONEST rather
// than aspirational: `general` and `preview` can be configured, `planned` and `deprecated` are
// labelled and refused. Without it the only way to keep the catalog truthful is to list nothing
// that does not work, and then a customer cannot see where the product is going; with it the
// list can be complete and still never waste an afternoon.

// Lifecycle is how far along a provider is.
//
// It is served text rather than a bitmask because an operator reads it and because it is the
// one field the frontend must branch on. It is NOT persisted: no connection row stores it, and
// a definition that changes lifecycle changes what may be created from now on rather than what
// already exists.
type Lifecycle string

const (
	// LifecycleGeneral is implemented, supported, and configurable.
	LifecycleGeneral Lifecycle = "general"
	// LifecyclePreview is implemented and configurable, with the support promise not yet made.
	LifecyclePreview Lifecycle = "preview"
	// LifecyclePlanned is named and not implemented. It appears in the catalog so an operator
	// can plan against it, and CreateConnection refuses it so nobody can try.
	LifecyclePlanned Lifecycle = "planned"
	// LifecycleDeprecated is implemented, still serving the Connections that exist, and closed
	// to new ones.
	LifecycleDeprecated Lifecycle = "deprecated"
)

// Actionable reports whether a Connection may be created against a definition in this state.
//
// This is the whole mechanism behind the honest catalog. It is checked in one place —
// Configurable, below, which handler and storage both go through — so a provider becoming
// actionable is one field in one record rather than a search for every place that guessed.
func (l Lifecycle) Actionable() bool {
	return l == LifecycleGeneral || l == LifecyclePreview
}

// Category is what kind of system a provider is, for grouping a catalog that will not stay
// small. It is the frontend's fallback icon too: where no approved mark exists, the neutral
// category icon is what gets drawn.
type Category string

const (
	CategoryOrchestration    Category = "orchestration"
	CategoryMetrics          Category = "metrics"
	CategoryLogs             Category = "logs"
	CategoryAlerting         Category = "alerting"
	CategoryIncidentResponse Category = "incident-response"
	CategoryCloud            Category = "cloud"
	CategoryVirtualisation   Category = "virtualisation"
	CategoryGeneric          Category = "generic"
)

// AuthMode is how a Connection to this provider proves who it is.
//
// The set is closed and shared across providers, because a setup flow that had to learn a new
// authentication vocabulary per provider is the schema-driven flow failing at the first thing
// it was built for.
type AuthMode string

const (
	// AuthSharedSecret is what an inbound trigger presents: a secret this platform mints, which
	// the customer configures in their own alerting.
	AuthSharedSecret AuthMode = "shared_secret"
	// AuthBearerToken is a token this platform presents outbound.
	AuthBearerToken AuthMode = "bearer_token"
	AuthBasic       AuthMode = "basic"
	// AuthServiceAccount is a Kubernetes service account the Relay runs as. There is no
	// credential to submit: the Relay already holds one, in the cluster, under the customer's
	// control, which is the point of running work there.
	AuthServiceAccount AuthMode = "service_account"
	AuthMutualTLS      AuthMode = "mutual_tls"
	// AuthRoleAssumption is AWS's, and it is the reason the presentation schema has an
	// extension slot: an external id and a trust policy cannot be collected by a generic form.
	AuthRoleAssumption AuthMode = "role_assumption"
	AuthAPIKey         AuthMode = "api_key"
)

// FieldType is what one configuration field holds. It is deliberately small: a setup form that
// needs a type this does not have is a provider that needs an extension slot, not a wider
// vocabulary.
type FieldType string

const (
	FieldString  FieldType = "string"
	FieldInteger FieldType = "integer"
	FieldBoolean FieldType = "boolean"
)

// Field is one thing a Connection to this provider is configured with.
//
// The configuration schema is RENDERED from these rather than written as JSON. That is what
// makes "every configurationSchema is a valid JSON Schema" true by construction instead of by a
// test that catches a typo after it ships, and it is what lets the presentation schema be
// checked against the fields that actually exist.
type Field struct {
	Name        string
	Title       string
	Description string
	Type        FieldType
	// Format is a JSON Schema format annotation — `uri`, `hostname`, `duration`. Empty means
	// the type is the whole constraint.
	Format   string
	Required bool
	// Secret marks a value that is written once and never read back. It renders as
	// `writeOnly: true`, and it is the schema saying what internal/connection/secret.go's rule
	// already says: what is stored is a reference and a digest, no path returns the value, and
	// an operator who loses it rotates rather than recovers.
	Secret bool
	// Enum closes a field to a named set, so a setup form renders a chooser rather than a text
	// box a customer can misspell.
	Enum []string
	// Default is what the field means when it is left out. It is rendered into the schema so a
	// form can show it rather than leaving the operator to discover it from behaviour.
	Default any
}

// Section is one group of fields in a setup form, in the order an operator meets them.
type Section struct {
	Title       string
	Description string
	// Fields names into the configuration schema. A name here that the schema does not declare
	// is a form asking for something nothing consumes, and the registry test refuses it.
	Fields []string
}

// Presentation is how to lay a configuration schema out, and where a generic form gives up.
//
// The generic half covers the ordinary case — an endpoint, an authentication method, a scope —
// and it covers it well enough that a new provider of that shape needs no frontend code at all.
// It will NOT cover AWS role assumption, Kubernetes service-account binding, or an OAuth
// consent round trip, and a form engine stretched until it does becomes unusable for the simple
// cases it was built for.
//
// ProviderStep is the seam. A provider with none needs no frontend work; a provider with one
// needs exactly the component its hard case requires and nothing more.
type Presentation struct {
	Sections []Section
	// ProviderStep is an identifier the frontend resolves to a purpose-built component. Empty
	// for every provider a generic form can serve, which is most of them.
	ProviderStep string
}

// ValidationContract says what validating a Connection to this provider can and cannot
// establish, BEFORE anybody runs one.
//
// It exists because a validation result that says "passed" without saying what was checked is
// the same kind of claim this product refuses everywhere else. An operator reading "connected"
// should be able to find out, from the catalog rather than from support, whether that meant a
// credential was exercised or a form was well-formed.
type ValidationContract struct {
	// Authenticates reports whether validation exercises the credential against the real
	// system. False means the check is structural, and the Note says so.
	Authenticates bool
	// ProbesCapabilities reports whether each declared capability is attempted separately, which
	// is what makes a partial result — "authentication worked, two of five capabilities are
	// unavailable" — a result rather than a failure.
	ProbesCapabilities bool
	// Note is what a successful validation does NOT prove, in the operator's language.
	Note string
}

// Definition is everything the product knows about one Integration.
type Definition struct {
	// Slug is the stable identity, and it is the value a connection row's `integration` column
	// stores. ID is served alongside it and is the same string: they are separate fields on the
	// wire so a client may key on one and route on the other, and they coincide because the
	// persisted value is already URL-safe. A future rename would change the slug and keep the
	// id, which is the whole reason to carry both.
	Slug string
	// DefinitionVersion is bumped when this record's SHAPE or meaning changes, so a client
	// built against an older definition can detect that it is stale rather than mis-render it.
	// It is not a version of the provider and not a version of its adapter.
	DefinitionVersion int
	Name              string
	Description       string
	Category          Category
	// LogoAssetKey names an approved mark in the frontend's brand registry. It carries no image
	// and it is ABSENT far more often than it is present, deliberately: the registry's rules
	// stand — no mark is redrawn, recoloured, cropped or generated — and where no approved
	// asset exists the neutral category icon is correct.
	//
	// A key is supplied only where the provider also has an adapter. A mark beside a provider
	// nobody can connect reads as a product further along than it is, which is the exact
	// dishonesty Lifecycle exists to remove; letting the logo follow the adapter keeps the two
	// signals telling the same story.
	LogoAssetKey     string
	DocumentationURL string
	AuthModes        []AuthMode
	// Roles is what a Connection to this provider may be used for. The specification calls this
	// `connectionModes`; it is served as `roles` because CONTEXT.md names this exact set the
	// Connection ROLE and expressly forbids "mode" as a synonym for it. Same set, the
	// vocabulary's own word.
	Roles               storage.ConnectionRole
	ExecutionLocalities []storage.ExecutionLocality
	// Configuration is what a Connection to this provider is configured with, and the source
	// the JSON Schema is rendered from.
	Configuration []Field
	Presentation  Presentation
	Validation    ValidationContract
	// Capabilities are the typed bounded reads connecting this provider makes available. They
	// are names internal/capability owns rather than strings of this file's own, because two
	// lists of capability names WILL disagree, and the one that is wrong will be the one an
	// operator was shown.
	Capabilities []string
	// MinimumRelayVersion is the oldest Relay that can serve this provider, and is stated by
	// every provider a Relay CAN serve — including the ones a customer may also reach centrally,
	// because the floor applies to whichever half of that choice they make. It is empty only for
	// a provider that is never relay-served and therefore has no floor to state.
	MinimumRelayVersion string
	// SupportsMultipleInstances reports whether an Organization may hold more than one
	// Connection of this Integration. It is true for every provider here, and it is a field
	// rather than an assumption because the first provider for which it is false will otherwise
	// be discovered by a customer.
	SupportsMultipleInstances bool
	Lifecycle                 Lifecycle
}

// ConfigurationSchema renders this definition's fields as JSON Schema draft 2020-12.
//
// It is rendered rather than authored so that a schema cannot be invalid, cannot drift from the
// presentation that lays it out, and cannot describe a field nothing reads.
func (d Definition) ConfigurationSchema() json.RawMessage {
	properties := make(map[string]any, len(d.Configuration))
	required := make([]string, 0, len(d.Configuration))

	for _, field := range d.Configuration {
		property := map[string]any{
			"type":        string(field.Type),
			"title":       field.Title,
			"description": field.Description,
		}
		if field.Format != "" {
			property["format"] = field.Format
		}
		if len(field.Enum) > 0 {
			property["enum"] = field.Enum
		}
		if field.Default != nil {
			property["default"] = field.Default
		}
		if field.Secret {
			// The schema says what the storage rule says: submitted, never returned.
			property["writeOnly"] = true
		}
		properties[field.Name] = property
		if field.Required {
			required = append(required, field.Name)
		}
	}
	sort.Strings(required)

	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://opencluster.dev/schemas/integration/" + d.Slug + "/configuration.json",
		"title":   d.Name + " configuration",
		"type":    "object",
		// Closed on purpose. A field a customer invented is a field nothing reads, and
		// accepting it silently is how a configuration comes to look complete and do nothing.
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	// A map assembled here cannot fail to encode: every value is a string, a bool, a number, a
	// []string or a map of those. An error would mean the shape above changed to something this
	// comment no longer describes, so it is reported as a schema rather than swallowed.
	encoded, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"error":"this definition's configuration could not be rendered"}`)
	}
	return encoded
}

// Field resolves one configuration field by name. It is the single lookup: Declares below reads
// it, and so does the check that decides whether a submitted value may be stored.
func (d Definition) Field(name string) (Field, bool) {
	for _, field := range d.Configuration {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

// Declares reports whether this definition has a configuration field by that name. It is what
// the presentation check and the configuration check both ask.
func (d Definition) Declares(name string) bool {
	_, declared := d.Field(name)
	return declared
}

// SecretField reports the one field that carries a credential, and whether there is one.
//
// One at most, by construction: a provider needing two credentials needs a provider step, not a
// second write-only field a generic form would render as two indistinguishable password boxes.
func (d Definition) SecretField() (Field, bool) {
	for _, field := range d.Configuration {
		if field.Secret {
			return field, true
		}
	}
	return Field{}, false
}

// Offers reports whether a Connection to this provider may serve every role asked of it.
func (d Definition) Offers(role storage.ConnectionRole) bool { return d.Roles.Includes(role) }

// Serves reports whether work against this provider may run in a given locality.
func (d Definition) Serves(locality storage.ExecutionLocality) bool {
	for _, offered := range d.ExecutionLocalities {
		if offered == locality {
			return true
		}
	}
	return false
}

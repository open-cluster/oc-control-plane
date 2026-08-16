// Package integrations owns the Integration domain: the catalog of Integration Types this
// product supports, and the configured installations belonging to an organization.
//
// An Integration Type is product-owned reference data. Its row in integration_type carries
// minimal catalog metadata and is seeded by migration; everything behavioral — configuration
// schema, capabilities, verification — lives in a provider package under this one, exported
// as a Definition and assembled into a Catalog at the composition root. The composition root
// is the only place that knows every provider; nothing here imports one.
//
// An Integration is one configured installation: "Production Alertmanager", "Acme Slack".
// org_id is the tenant boundary. There is no Environment, no Connection role, and no
// execution locality — where work runs is derivable from whether a Relay serves the
// integration.
package integrations

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// TypeID is the persisted identity of an Integration Type. The values are frozen: they are
// seeded into integration_type by migration and compiled here as constants, so no runtime
// lookup maps a key to an id. A test proves the two sets agree.
type TypeID int16

const (
	TypeAlertmanager TypeID = 1
	TypeKubernetes   TypeID = 2
)

// Category groups the catalog. A controlled vocabulary owned here; deliberately not a table.
type Category string

const (
	CategoryAlerting      Category = "alerting"
	CategoryOrchestration Category = "orchestration"
	CategoryObservability Category = "observability"
	CategoryCollaboration Category = "collaboration"
	CategorySourceControl Category = "source-control"
	CategoryIncident      Category = "incident-response"
	CategoryDeployment    Category = "deployment"
	CategoryCloud         Category = "cloud"
)

// FieldType is what one configuration field holds. Deliberately small: a setup form that
// needs a type this does not have is a provider that needs its own component, not a wider
// vocabulary.
type FieldType string

const (
	FieldString  FieldType = "string"
	FieldInteger FieldType = "integer"
	FieldBoolean FieldType = "boolean"
)

// Field is one thing an Integration of this type is configured with. The configuration
// schema is RENDERED from these rather than written as JSON, which is what makes "every
// configuration schema is valid" true by construction.
type Field struct {
	Name        string
	Title       string
	Description string
	Type        FieldType
	// Format is a JSON Schema format annotation — `uri`, `hostname`. Empty means the type
	// is the whole constraint.
	Format   string
	Required bool
	// Secret marks a value that is written once and never read back. No provider in this
	// build declares one; the field exists so the schema can say writeOnly when one does.
	Secret bool
	// Enum closes a field to a named set.
	Enum []string
	// Default is what the field means when it is left out.
	Default any
}

// Verification is what a verify run established.
type Verification struct {
	Status Status
	// Note says, in the operator's language, what this run proved or could not.
	Note string
}

// VerifyInput is everything a Definition's Verify may consult. It is gathered by the
// handler so the verification itself is a pure function of observed facts.
type VerifyInput struct {
	Integration Integration
	// Relay is the state of the bound Relay, meaningful only when the type requires one.
	Relay RelayStatus
	// LastAcceptedDelivery is when a webhook-receiving integration last accepted a real
	// delivery, zero when it never has.
	LastAcceptedDelivery time.Time
}

// RelayStatus is what verification may know about the Relay serving an integration.
type RelayStatus struct {
	// Bound reports whether the integration names a Relay at all.
	Bound bool
	// Connected reports whether that Relay currently holds a session.
	Connected bool
	// Capabilities is what the Relay advertised at enrolment.
	Capabilities []string
}

// Definition is everything one provider package exports about its Integration Type.
// Metadata mirrors the seeded integration_type row; behavior is the provider's own.
type Definition struct {
	ID          TypeID
	Key         string
	Name        string
	Description string
	// Logo names an approved mark in the frontend's brand registry. Empty means the
	// neutral category icon is drawn.
	Logo             string
	Category         Category
	DocumentationURL string
	// Capabilities are the named operations connecting this type makes available.
	Capabilities []string
	// Config is what an Integration of this type is configured with, and the source its
	// JSON Schema is rendered from.
	Config []Field
	// RequiresRelay means an Integration of this type must be bound to a Relay.
	RequiresRelay bool
	// ReceivesWebhooks means an Integration of this type is reached inbound: creating one
	// mints a webhook secret, and the intake surface accepts deliveries for it.
	ReceivesWebhooks bool
	// MinimumRelayVersion is the oldest Relay that can serve this type; empty for a type
	// no Relay serves.
	MinimumRelayVersion string
	// Verify judges an integration against the facts in VerifyInput. It is pure: the
	// handler gathers, the definition judges, the store records.
	Verify func(VerifyInput) Verification
}

// ConfigurationSchema renders this definition's fields as JSON Schema draft 2020-12.
func (d Definition) ConfigurationSchema() json.RawMessage {
	properties := make(map[string]any, len(d.Config))
	required := make([]string, 0, len(d.Config))

	for _, field := range d.Config {
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
		"$id":     "https://opencluster.dev/schemas/integration/" + d.Key + "/configuration.json",
		"title":   d.Name + " configuration",
		"type":    "object",
		// Closed on purpose. A field a customer invented is a field nothing reads, and
		// accepting it silently is how a configuration comes to look complete and do
		// nothing.
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"error":"this definition's configuration could not be rendered"}`)
	}
	return encoded
}

// Field resolves one configuration field by name. It is the single lookup: Declares reads
// it, and so does the check that decides whether a submitted value may be stored.
func (d Definition) Field(name string) (Field, bool) {
	for _, field := range d.Config {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

// Declares reports whether this definition has a configuration field by that name.
func (d Definition) Declares(name string) bool {
	_, declared := d.Field(name)
	return declared
}

// Catalog is the assembled set of Definitions this deployment serves. It is built once at
// the composition root and read everywhere else.
type Catalog struct {
	ordered []Definition
	byKey   map[string]Definition
	byID    map[TypeID]Definition
}

// NewCatalog assembles and validates the definitions. A duplicate key or id, a definition
// with no Verify, or one with neither an inbound nor an outbound shape is a programming
// error and refuses assembly — at startup, where the person who caused it is reading.
func NewCatalog(definitions ...Definition) (Catalog, error) {
	catalog := Catalog{
		ordered: make([]Definition, 0, len(definitions)),
		byKey:   make(map[string]Definition, len(definitions)),
		byID:    make(map[TypeID]Definition, len(definitions)),
	}
	for _, definition := range definitions {
		switch {
		case definition.Key == "" || definition.ID == 0:
			return Catalog{}, fmt.Errorf("integration definition %q has no identity", definition.Key)
		case definition.Verify == nil:
			return Catalog{}, fmt.Errorf("integration type %q declares no verification", definition.Key)
		}
		if _, taken := catalog.byKey[definition.Key]; taken {
			return Catalog{}, fmt.Errorf("integration type key %q is declared twice", definition.Key)
		}
		if _, taken := catalog.byID[definition.ID]; taken {
			return Catalog{}, fmt.Errorf("integration type id %d is declared twice", definition.ID)
		}
		catalog.ordered = append(catalog.ordered, definition)
		catalog.byKey[definition.Key] = definition
		catalog.byID[definition.ID] = definition
	}
	sort.Slice(catalog.ordered, func(i, j int) bool {
		return catalog.ordered[i].Key < catalog.ordered[j].Key
	})
	return catalog, nil
}

// All returns every definition, ordered by key so a rendered catalog is stable.
func (c Catalog) All() []Definition { return append([]Definition(nil), c.ordered...) }

// Lookup resolves a definition from its stable key.
func (c Catalog) Lookup(key string) (Definition, bool) {
	definition, ok := c.byKey[key]
	return definition, ok
}

// ByID resolves a definition from its persisted type id.
func (c Catalog) ByID(id TypeID) (Definition, bool) {
	definition, ok := c.byID[id]
	return definition, ok
}

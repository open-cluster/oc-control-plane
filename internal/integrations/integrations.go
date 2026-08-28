// Package integrations owns the Integration domain: the catalog of Integration Types this
// product supports, and the configured installations belonging to an organization.
//
// An Integration Type is product-owned reference data. Its row in integration_type carries
// minimal catalog metadata and is seeded by migration; everything behavioral — configuration
// schema, verification, and Tools — lives in a provider package under this one, exported
// as a Definition and assembled into a Catalog at the composition root. The composition root
// is the only place that knows every provider; nothing here imports one.
//
// An Integration is one configured installation: "Production Alertmanager", "Org Slack".
// org_id is the tenant boundary. Where work runs is derivable from whether a Relay serves
// the integration.
package integrations

import (
	"context"
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
	TypeAlertmanager   TypeID = 1
	TypeKubernetes     TypeID = 2
	TypeSlack          TypeID = 3
	TypeGitHub         TypeID = 4
	TypeGenericWebhook TypeID = 5
)

// Category groups the catalog. A controlled vocabulary owned here; deliberately not a table.
type Category string

const (
	CategoryAlerting       Category = "alerting"
	CategoryInfrastructure Category = "infrastructure"
	CategoryCollaboration  Category = "collaboration"
	CategorySourceControl  Category = "source-control"
)

// FieldType is what one configuration field holds. Deliberately small: a setup form that
// needs a type this does not have is a provider that needs its own component, not a wider
// vocabulary.
type FieldType string

const (
	FieldString  FieldType = "string"
	FieldInteger FieldType = "integer"
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
	// Secret marks a value that is written once and never read back: it is routed to the
	// sealer, never to configuration, and the rendered schema says writeOnly. A definition
	// declaring one must declare a Probe, because the only honest check of a credential is
	// presenting it to the provider.
	Secret bool
	// Recorded marks a value the INSTALLATION FLOW writes and a caller never may. It is
	// declared so that an operator reading the record can see it and a schema can describe
	// it, and it is refused on the way in — a field that only a proven connect can set
	// must not be typeable, or a claim the flow established becomes a claim anybody with
	// update permission can assert.
	//
	// The rendered schema says readOnly, which is exactly what it means.
	Recorded bool
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
	// Grants are facts the probe verified about the credential, in the provider's own
	// vocabulary — Slack records granted scopes plus the token's kind. Tool
	// availability derives from them: a tool whose Requires are not all recorded here
	// is absent from an investigation's set instead of failing at call time. Nil means
	// nothing was recorded, and gated tools stay absent.
	Grants []string
	// Facts are non-secret, provider-shaped things this run established about what is
	// connected — GitHub records the account, its type, and how far the installation's
	// repository grant reaches. They are for display and for support and are never
	// consulted by an authorization decision, which is what keeps them separate from
	// Grants. Nil means the run established none.
	Facts map[string]any
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
	// Capabilities is the Relay Capabilities advertised at enrolment.
	Capabilities []string
}

// ProbeInput is what a live outbound verification is given: the integration as recorded —
// or as it is about to be recorded, at creation — and the plaintext credential, unsealed
// for this one call and never stored by anything downstream of it. Empty for a type whose
// probe authenticates with deployment-level credentials instead.
type ProbeInput struct {
	Integration Integration
	Credential  string
}

// InboundAvailability describes whether an installed Integration can receive its
// provider-specific inbound interaction, independently of investigation Tools.
type InboundAvailability struct {
	Available bool
	Reason    string
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
	Logo     string
	Category Category
	// DocumentationURL points at the VENDOR's own setup documentation: Slack's token
	// types, Prometheus's webhook_config reference, Kubernetes RBAC. Useful, and not the
	// page an operator setting this up actually needs first.
	//
	// Ours is ProductDocumentationURL, and it is derived rather than declared here — see
	// the method for why a hand-written second URL per provider was the wrong shape.
	DocumentationURL string
	// Config is what an Integration of this type is configured with, and the source its
	// JSON Schema is rendered from.
	Config []Field
	// RequiresRelay means an Integration of this type must be bound to a Relay.
	RequiresRelay bool
	// ReceivesWebhooks means an Integration of this type is reached inbound: creating one
	// mints a webhook secret, and the intake surface accepts deliveries for it.
	ReceivesWebhooks bool
	// Verify judges an integration against the facts in VerifyInput. It is pure: the
	// handler gathers, the definition judges, the store records. A definition declares
	// exactly one of Verify and Probe.
	Verify func(VerifyInput) Verification
	// Probe verifies live against the provider: the far end is asked, and the judgement
	// comes back with what it answered. It is the verification for every outbound type,
	// because a credential's only honest check is presenting it.
	Probe func(ctx context.Context, input ProbeInput) Verification
	// Tools are the bounded reads connecting this type makes available, rendered in the
	// catalog with the routing guidance each declares.
	Tools []Tool
	// Inbound judges a provider-specific interactive endpoint against deployment setup
	// and recorded installation facts. Nil means the provider declares no such endpoint.
	Inbound func(Integration) InboundAvailability
	// Connect is the provider's own installation flow, when this deployment can offer
	// one. Nil means the type is connected through its configuration form — which is
	// what a self-hosted deployment that registered no application with the vendor has,
	// and it stays supported.
	Connect *Connect
}

// Manifest is the persisted and displayed metadata owned by a provider. Behavior remains
// on Definition; this projection is the single input for database reconciliation, docs,
// and clients that render the catalog.
type Manifest struct {
	ID          TypeID
	Key         string
	Name        string
	Description string
	Logo        string
	Category    Category
}

// documentationSite is where this product's own documentation is published. One constant,
// beside the schema $id's origin above, because the site is the product's and not a
// deployment's: a self-hosted install reads the same published pages.
const documentationSite = "https://docs.opencluster.dev"

// ProductDocumentationURL is OUR page for this type — the one that carries the receiver
// YAML, the header name and the version floor, rather than the vendor's reference.
//
// DERIVED, NEVER TYPED. The documentation gate already asserts that every shipped type has
// a page at docs/integrations/<role>/<key>.mdx, and role is the Category and key is the
// Key — so the definition already knows where its own page is, and asking each provider to
// write the URL out again would be asking for the one copy that drifts. A page moved
// without this being updated fails the gate; a type added without a page fails both the
// gate and the catalog test.
func (d Definition) ProductDocumentationURL() string {
	if d.Key == "" || d.Category == "" {
		return ""
	}
	return documentationSite + "/integrations/" + string(d.Category) + "/" + d.Key
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
		if field.Recorded {
			property["readOnly"] = true
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

// SecretField resolves this definition's one credential field, when it declares one.
func (d Definition) SecretField() (Field, bool) {
	for _, field := range d.Config {
		if field.Secret {
			return field, true
		}
	}
	return Field{}, false
}

// Catalog is the assembled set of Definitions this deployment serves. It is built once at
// the composition root and read everywhere else.
type Catalog struct {
	ordered []Definition
	byKey   map[string]Definition
	byID    map[TypeID]Definition
}

// NewCatalog assembles and validates the definitions. A duplicate key or id, a definition
// with no verification or two kinds of it, a second secret field, a credential without a
// probe to check it, or an incomplete Tool contract — each is a
// programming error and refuses assembly, at startup, where the person who caused it is
// reading.
func NewCatalog(definitions ...Definition) (Catalog, error) {
	catalog := Catalog{
		ordered: make([]Definition, 0, len(definitions)),
		byKey:   make(map[string]Definition, len(definitions)),
		byID:    make(map[TypeID]Definition, len(definitions)),
	}
	for _, definition := range definitions {
		if definition.Key == "" || definition.ID == 0 {
			return Catalog{}, fmt.Errorf("integration definition %q has no identity", definition.Key)
		}
		if err := checkDefinition(definition); err != nil {
			return Catalog{}, err
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

// checkDefinition refuses a definition whose declarations cannot all be true at once.
func checkDefinition(definition Definition) error {
	if (definition.Verify == nil) == (definition.Probe == nil) {
		return fmt.Errorf("integration type %q must declare exactly one of Verify and Probe",
			definition.Key)
	}

	secrets := 0
	for _, field := range definition.Config {
		if field.Secret {
			secrets++
		}
	}
	if secrets > 1 {
		return fmt.Errorf("integration type %q declares %d secret fields; one credential "+
			"is the model, and a second one is a second thing to seal, replace and audit",
			definition.Key, secrets)
	}
	if secrets == 1 && definition.Probe == nil {
		return fmt.Errorf("integration type %q takes a credential and declares no probe; "+
			"an uncheckable credential would make \"verified\" a form having validated",
			definition.Key)
	}

	names := make(map[string]bool, len(definition.Tools))
	for _, tool := range definition.Tools {
		switch {
		case tool.Name == "" || tool.Run == nil:
			return fmt.Errorf("integration type %q declares a tool without a name or a run",
				definition.Key)
		case names[tool.Name]:
			return fmt.Errorf("integration type %q declares tool %q twice",
				definition.Key, tool.Name)
		case tool.Description == "" || tool.WhenToUse == "" || tool.WhenNotToUse == "" ||
			tool.Permissions == "" || tool.Output == "":
			return fmt.Errorf("integration type %q tool %q is missing part of its "+
				"contract; the model routes by the composed description, and an empty "+
				"field is a tool that gets used wrongly", definition.Key, tool.Name)
		}
		if err := checkArguments(definition.Key, tool); err != nil {
			return err
		}
		names[tool.Name] = true
	}
	return nil
}

// checkArguments refuses a tool whose argument declarations are incomplete. The model
// plans calls by these declarations, so a nameless, undescribed or untyped argument is
// a call that goes wrong at three in the morning instead of failing this build.
func checkArguments(key string, tool Tool) error {
	declared := make(map[string]bool, len(tool.Arguments))
	for _, argument := range tool.Arguments {
		switch {
		case argument.Name == "" || argument.Description == "":
			return fmt.Errorf("integration type %q tool %q declares an argument without "+
				"a name or a description", key, tool.Name)
		case argument.Type != FieldString && argument.Type != FieldInteger:
			return fmt.Errorf("integration type %q tool %q argument %q has type %q, "+
				"which is not one this catalog serves", key, tool.Name,
				argument.Name, argument.Type)
		case declared[argument.Name]:
			return fmt.Errorf("integration type %q tool %q declares argument %q twice",
				key, tool.Name, argument.Name)
		}
		declared[argument.Name] = true
	}
	return nil
}

// All returns every definition, ordered by key so a rendered catalog is stable.
func (c Catalog) All() []Definition { return append([]Definition(nil), c.ordered...) }

func (c Catalog) Manifests() []Manifest {
	manifests := make([]Manifest, 0, len(c.ordered))
	for _, definition := range c.ordered {
		manifests = append(manifests, Manifest{ID: definition.ID, Key: definition.Key,
			Name: definition.Name, Description: definition.Description, Logo: definition.Logo,
			Category: definition.Category})
	}
	return manifests
}

// Tools returns every declared Tool in stable Integration Type and declaration order.
func (c Catalog) Tools() []Tool {
	var tools []Tool
	for _, definition := range c.ordered {
		tools = append(tools, definition.Tools...)
	}
	return tools
}

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

// CredentialBearing reports the keys of the types that take a pasted credential, in
// order. The composition root refuses to serve the operator surface for a non-empty
// answer with no sealing key configured: the setup flow would accept a secret it could
// only store in the clear or drop.
func (c Catalog) CredentialBearing() []string {
	var keys []string
	for _, definition := range c.ordered {
		if _, holds := definition.SecretField(); holds {
			keys = append(keys, definition.Key)
		}
	}
	return keys
}

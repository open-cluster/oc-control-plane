package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type TypeID int16

const (
	TypeAlertmanager   TypeID = 1
	TypeKubernetes     TypeID = 2
	TypeSlack          TypeID = 3
	TypeGitHub         TypeID = 4
	TypeGenericWebhook TypeID = 5
)

type Category string

const (
	CategoryAlerting       Category = "alerting"
	CategoryInfrastructure Category = "infrastructure"
	CategoryCollaboration  Category = "collaboration"
	CategorySourceControl  Category = "source-control"
)

type FieldType string

const (
	FieldString  FieldType = "string"
	FieldInteger FieldType = "integer"
)

type Field struct {
	Name        string
	Title       string
	Description string
	Type        FieldType
	Format      string
	Required    bool
	Secret      bool
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
type VerifyInput struct {
	Integration          Integration
	RelayStatus          RelayStatus
	LastAcceptedDelivery time.Time
}

// RelayStatus is what verification may know about the Relay serving an integration.
type RelayStatus struct {
	Bound        bool
	Connected    bool
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
// Its manifest owns catalog metadata; behavior is the provider's own.
type Definition struct {
	Manifest
	// Verify judges an integration against the facts in VerifyInput. It is pure: the
	// handler gathers, the definition judges, the store records. A definition declares
	// exactly one of Verify and Probe.
	Verify func(VerifyInput) Verification
	// Probe verifies live against the provider: the far end is asked, and the judgement
	// comes back with what it answered. It is the verification for every outbound type,
	// because a credential's only honest check is presenting it.
	Probe func(ctx context.Context, input ProbeInput) Verification
	// Inbound judges a provider-specific interactive endpoint against deployment setup
	// and recorded installation facts. Nil means the provider declares no such endpoint.
	Inbound func(Integration) InboundAvailability
	// Connect is the provider's own installation flow, when this deployment can offer
	// one. Nil means the type is connected through its configuration form — which is
	// what a self-hosted deployment that registered no application with the vendor has,
	// and it stays supported.
	Connect *Connect
}

// Manifest is the authoritative provider declaration used by runtime routing, database
// reconciliation, docs, and clients that render the catalog.
type Manifest struct {
	ID                TypeID
	Key               string
	Name              string
	Description       string
	Logo              string
	Category          Category
	Available         bool
	DocumentationSlug string
	SourceURL         string
	ReceivesWebhooks  bool
	RequiresRelay     bool
	SupportsConnect   bool
	Config            []Field
	Tools             []Tool
}

// documentationSite is where this product's own documentation is published. One constant,
// beside the schema $id's origin above, because the site is the product's and not a
// deployment's: a self-hosted install reads the same published pages.
const documentationSite = "https://docs.open-cluster.io"

// ProductDocumentationURL is OUR page for this type — the one that carries the receiver
// YAML, the header name and the version floor, rather than the vendor's reference.
//
// DocumentationSlug is declared beside the rest of the provider metadata and the catalog
// rejects any non-empty value that differs from integrations/<category>/<key>. The product
// documentation gate additionally requires every shipped provider to declare that canonical
// slug and verifies the corresponding page exists.
func (m Manifest) ProductDocumentationURL() string {
	if m.DocumentationSlug == "" {
		return ""
	}
	return documentationSite + "/" + m.DocumentationSlug
}

// ConfigurationSchema renders this definition's fields as JSON Schema draft 2020-12.
func (m Manifest) ConfigurationSchema() json.RawMessage {
	properties := make(map[string]any, len(m.Config))
	required := make([]string, 0, len(m.Config))

	for _, field := range m.Config {
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
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://opencluster.dev/schemas/integration/" + m.Key + "/configuration.json",
		"title":                m.Name + " configuration",
		"type":                 "object",
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

// Capabilities returns the stable tool names this provider makes available.
func (m Manifest) Capabilities() []string {
	capabilities := make([]string, 0, len(m.Tools))
	for _, tool := range m.Tools {
		capabilities = append(capabilities, tool.Name)
	}
	return capabilities
}

// SecretFields returns configuration field names whose values are sealed at rest.
func (m Manifest) SecretFields() []string {
	fields := make([]string, 0, len(m.Config))
	for _, field := range m.Config {
		if field.Secret {
			fields = append(fields, field.Name)
		}
	}
	return fields
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

type Catalog struct {
	ordered []Definition
	byKey   map[string]Definition
	byID    map[TypeID]Definition
}

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

func checkDefinition(definition Definition) error {
	wantDocumentationSlug := "integrations/" + string(definition.Category) + "/" + definition.Key
	if definition.DocumentationSlug != "" && definition.DocumentationSlug != wantDocumentationSlug {
		return fmt.Errorf("integration type %q documentation slug is %q, want %q",
			definition.Key, definition.DocumentationSlug, wantDocumentationSlug)
	}
	if definition.SupportsConnect != definition.Connectable() {
		return fmt.Errorf("integration type %q manifest connect availability does not match its behavior", definition.Key)
	}
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

func (c Catalog) All() []Definition { return append([]Definition(nil), c.ordered...) }

func (c Catalog) Manifests() []Manifest {
	manifests := make([]Manifest, 0, len(c.ordered))
	for _, definition := range c.ordered {
		manifest := definition.Manifest
		manifest.Config = append([]Field(nil), manifest.Config...)
		manifest.Tools = append([]Tool(nil), manifest.Tools...)
		for index := range manifest.Tools {
			manifest.Tools[index].Arguments = append([]ToolArgument(nil), manifest.Tools[index].Arguments...)
			manifest.Tools[index].Requires = append([]string(nil), manifest.Tools[index].Requires...)
		}
		manifests = append(manifests, manifest)
	}
	return manifests
}

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

func (c Catalog) CredentialBearing() []string {
	var keys []string
	for _, definition := range c.ordered {
		if _, holds := definition.SecretField(); holds {
			keys = append(keys, definition.Key)
		}
	}
	return keys
}

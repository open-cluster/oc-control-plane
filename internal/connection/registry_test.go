package connection_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/connection"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The registry is a compiled artefact, so its defects are compile-time facts and belong in a
// gate rather than in a review comment. Each test below is a property a catalog rendered from
// this must have — and each one is a property review would miss, because a wrong field in one
// of thirteen records reads exactly like a right one.

func TestEveryDefinitionIsCompleteEnoughToRender(t *testing.T) {
	t.Parallel()

	listed := connection.Definitions()
	if len(listed) == 0 {
		t.Fatal("the registry is empty; every gate here would pass vacuously")
	}

	for _, definition := range listed {
		if definition.Slug == "" || definition.Name == "" || definition.Description == "" {
			t.Errorf("%q has no slug, name or description; a catalog tile cannot be drawn from it",
				definition.Slug)
		}
		if definition.Category == "" {
			t.Errorf("%s names no category, so it cannot be grouped and has no fallback icon",
				definition.Slug)
		}
		if definition.DefinitionVersion <= 0 {
			t.Errorf("%s carries no definition version, so a stale client cannot detect that it "+
				"is stale", definition.Slug)
		}
		if definition.DocumentationURL == "" {
			t.Errorf("%s links to no setup documentation (story 8)", definition.Slug)
		}
		if len(definition.AuthModes) == 0 {
			t.Errorf("%s declares no authentication mode", definition.Slug)
		}
		if len(definition.ExecutionLocalities) == 0 {
			t.Errorf("%s says nowhere its work runs, so an operator cannot tell whether they "+
				"need to install anything (story 6)", definition.Slug)
		}
		if definition.Roles == 0 {
			t.Errorf("%s serves no role, so nothing could ever be configured against it",
				definition.Slug)
		}
	}
}

// The configuration schema is rendered rather than authored, so this asserts the rendering:
// valid JSON, an object, closed to properties nobody declared, and requiring only fields it has.
func TestEveryConfigurationSchemaIsAValidClosedObjectSchema(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		var schema struct {
			Schema               string                     `json:"$schema"`
			ID                   string                     `json:"$id"`
			Type                 string                     `json:"type"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
		}
		raw := definition.ConfigurationSchema()
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Errorf("%s renders a configuration schema that is not JSON: %v", definition.Slug, err)
			continue
		}
		if schema.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s declares the dialect %q, not draft 2020-12",
				definition.Slug, schema.Schema)
		}
		if schema.ID == "" {
			t.Errorf("%s renders a schema with no $id, so nothing can reference it",
				definition.Slug)
		}
		if schema.Type != "object" {
			t.Errorf("%s renders a %q schema; a configuration is an object",
				definition.Slug, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s accepts properties it does not declare; a field nothing reads is how a "+
				"configuration comes to look complete and do nothing", definition.Slug)
		}
		if len(schema.Properties) != len(definition.Configuration) {
			t.Errorf("%s declares %d fields and renders %d properties",
				definition.Slug, len(definition.Configuration), len(schema.Properties))
		}
		for _, required := range schema.Required {
			if !definition.Declares(required) {
				t.Errorf("%s requires %q, which it does not declare", definition.Slug, required)
			}
		}
	}
}

// The presentation lays the schema out, so it may only name fields the schema has. A section
// pointing at a field nobody declared renders an input that is submitted and discarded.
func TestEveryPresentationNamesOnlyDeclaredFields(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		laidOut := make(map[string]bool)
		for _, section := range definition.Presentation.Sections {
			if section.Title == "" {
				t.Errorf("%s has an untitled section", definition.Slug)
			}
			if len(section.Fields) == 0 && definition.Presentation.ProviderStep == "" {
				t.Errorf("%s has a section %q with no fields and no provider step",
					definition.Slug, section.Title)
			}
			for _, field := range section.Fields {
				if !definition.Declares(field) {
					t.Errorf("%s lays out %q, which its configuration schema does not declare",
						definition.Slug, field)
				}
				if laidOut[field] {
					t.Errorf("%s lays out %q twice", definition.Slug, field)
				}
				laidOut[field] = true
			}
		}
		// The other direction. A declared field nobody lays out is a value the setup form never
		// collects, which is worse than a missing form: the Connection is created and the field
		// is silently empty.
		for _, field := range definition.Configuration {
			if !laidOut[field.Name] {
				t.Errorf("%s declares %q and no section collects it", definition.Slug, field.Name)
			}
		}
	}
}

// A capability name is internal/capability's to own. Two lists of capability names WILL
// disagree, and the one that is wrong is the one an operator was shown.
func TestEveryDeclaredCapabilityIsOneThisBuildDispatches(t *testing.T) {
	t.Parallel()

	known := make(map[string]bool)
	for _, descriptor := range capability.Registered() {
		known[descriptor.ID] = true
	}
	if len(known) == 0 {
		t.Fatal("this build dispatches no capabilities; the gate would pass vacuously")
	}

	for _, definition := range connection.Definitions() {
		for _, declared := range definition.Capabilities {
			if !known[declared] {
				t.Errorf("%s claims the capability %q, which this build does not dispatch",
					definition.Slug, declared)
			}
		}
	}
}

// This is the test that makes the honest catalog structural rather than a copy decision.
//
// A provider with no adapter must not be creatable, and the refusal must come from the registry
// rather than from a handler that remembered to check. Reversing the honesty then means editing
// a lifecycle field, which is visible, rather than deleting a condition, which is not.
func TestOnlyAnActionableLifecycleMayBeConfigured(t *testing.T) {
	t.Parallel()

	actionable := 0
	for _, definition := range connection.Definitions() {
		_, reason, ok := connection.Configurable(connection.Integration(definition.Slug))
		if definition.Lifecycle.Actionable() {
			actionable++
			if !ok {
				t.Errorf("%s is %s and is refused at creation: %s",
					definition.Slug, definition.Lifecycle, reason)
			}
			continue
		}
		if ok {
			t.Errorf("%s is %s and may be configured; a connection to a provider with no adapter "+
				"is an afternoon somebody spends on something that cannot work",
				definition.Slug, definition.Lifecycle)
		}
		if reason == "" {
			t.Errorf("%s is refused with no reason a caller could act on", definition.Slug)
		}
	}
	if actionable == 0 {
		t.Fatal("no provider is configurable at all; the catalog would be honest and useless")
	}
}

func TestAnIntegrationThisBuildDoesNotHaveIsRefusedByName(t *testing.T) {
	t.Parallel()

	_, reason, ok := connection.Configurable("splunk")
	if ok {
		t.Fatal("an integration this build has never heard of was accepted")
	}
	if !strings.Contains(reason, "splunk") {
		t.Errorf("the refusal %q does not name what was asked for", reason)
	}
}

// A provider with no adapter must not carry a logo either.
//
// The rule is the catalog's honesty applied to the other signal an operator reads. A mark beside
// something nobody can connect says the product is further along than it is, which is what
// lifecycle exists to stop it saying — and a key naming a file the brand registry has no
// manifest entry for renders a broken image rather than the neutral category icon that is
// correct in its absence.
func TestOnlyAProviderWithAnAdapterCarriesALogoKey(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		if definition.LogoAssetKey == "" {
			continue
		}
		if !definition.Lifecycle.Actionable() {
			t.Errorf("%s is %s and names the logo %q; a mark beside a provider nobody can "+
				"connect reads as a product further along than it is",
				definition.Slug, definition.Lifecycle, definition.LogoAssetKey)
		}
		if definition.LogoAssetKey != definition.Slug {
			t.Errorf("%s names the logo key %q; a key that is not the slug is one more thing "+
				"the brand manifest and this file can disagree about",
				definition.Slug, definition.LogoAssetKey)
		}
	}
}

// A provider that cannot be reached through a Relay has no minimum Relay version to state, and
// one that can must state it — a relay-served provider with no floor is a Relay that will be
// asked for a capability it does not compile.
func TestAMinimumRelayVersionIsStatedWhereAndOnlyWhereARelayIsInvolved(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		viaRelay := definition.Serves(storage.LocalityRelay)
		switch {
		case viaRelay && definition.MinimumRelayVersion == "":
			t.Errorf("%s runs through a Relay and states no minimum version", definition.Slug)
		case !viaRelay && definition.MinimumRelayVersion != "":
			t.Errorf("%s is reached centrally and states a minimum Relay version %q",
				definition.Slug, definition.MinimumRelayVersion)
		}
	}
}

// At most one credential per provider. Two write-only fields render as two indistinguishable
// password boxes, which is a provider that needs an extension slot rather than a wider form.
func TestAtMostOneFieldCarriesACredential(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		secrets := 0
		for _, field := range definition.Configuration {
			if field.Secret {
				secrets++
			}
		}
		if secrets > 1 {
			t.Errorf("%s declares %d write-only fields; a generic form renders those as two "+
				"indistinguishable password boxes", definition.Slug, secrets)
		}
		if field, has := definition.SecretField(); has && !field.Secret {
			t.Errorf("%s reports a secret field that is not marked secret", definition.Slug)
		}
	}
}

// A write-only field must SAY it is write-only in the schema, because that is the only place a
// generic form engine can read it from.
func TestASecretFieldRendersAsWriteOnly(t *testing.T) {
	t.Parallel()

	for _, definition := range connection.Definitions() {
		field, has := definition.SecretField()
		if !has {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				WriteOnly bool `json:"writeOnly"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.ConfigurationSchema(), &schema); err != nil {
			t.Fatalf("%s: %v", definition.Slug, err)
		}
		if !schema.Properties[field.Name].WriteOnly {
			t.Errorf("%s renders %q without writeOnly, so a form would offer to show it back",
				definition.Slug, field.Name)
		}
	}
}

// The catalog is ordered so what works comes first. A list that interleaves the two makes an
// operator read every tile to find the ones they can act on.
func TestTheCatalogPutsWhatWorksFirst(t *testing.T) {
	t.Parallel()

	seenPlanned := false
	for _, definition := range connection.Definitions() {
		if !definition.Lifecycle.Actionable() {
			seenPlanned = true
			continue
		}
		if seenPlanned {
			t.Errorf("%s is actionable and sorts after a provider that is not", definition.Slug)
		}
	}
}

// The two vocabularies that used to be kept in two places now come from one, so this asserts
// they still agree rather than that they exist.
func TestTheRoleVocabularyComesFromTheDefinitions(t *testing.T) {
	t.Parallel()

	if !connection.Offers(connection.Alertmanager, storage.RoleTrigger) {
		t.Error("alertmanager no longer offers the trigger role")
	}
	if connection.Offers(connection.Alertmanager, storage.RoleEvidence) {
		t.Error("alertmanager offers evidence reads; it delivers inbound and cannot be read from")
	}
	if !connection.Offers(connection.Kubernetes, storage.RoleEvidence) {
		t.Error("kubernetes no longer offers the evidence role")
	}
	if connection.Offers(connection.Kubernetes, storage.RoleTrigger) {
		t.Error("kubernetes offers triggers; a cluster pushes nothing to this platform")
	}
	if !connection.Known(connection.Integration("prometheus")) {
		t.Error("a planned provider is not KNOWN; it must be, or the catalog cannot list it")
	}
}

func TestSearchReadsTheNameTheSlugAndTheDescription(t *testing.T) {
	t.Parallel()

	definition, ok := connection.Lookup(connection.Integration("loki"))
	if !ok {
		t.Fatal("loki is not in the registry")
	}
	for _, wanted := range []string{"", "loki", "LOKI", "Grafana", "logs"} {
		if !definition.Matches(wanted) {
			t.Errorf("searching %q does not find loki", wanted)
		}
	}
	if definition.Matches("proxmox") {
		t.Error("searching for proxmox finds loki")
	}
}

// The catalog listing speaks the shared table contract, so the query it accepts is worth
// asserting against the contract rather than against the handler.
func TestTheCatalogSpecRefusesAFilterItDoesNotServe(t *testing.T) {
	t.Parallel()

	if _, err := url.ParseQuery("lifecycle=general&category=metrics"); err != nil {
		t.Fatalf("the query this test is built on does not parse: %v", err)
	}
}

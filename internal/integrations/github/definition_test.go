package github

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The definition's promises, pinned. These are what the seeded reference row, the setup
// flow and the investigator rely on; a drift here is a product change, not a refactor.

func TestDefinitionMirrorsTheSeededRow(t *testing.T) {
	t.Parallel()

	definition := Definition(nil, NewClient(""))
	if definition.ID != integrations.TypeGitHub || definition.Key != "github" {
		t.Errorf("identity = %d %q", definition.ID, definition.Key)
	}
	if definition.Category != integrations.CategorySourceControl {
		t.Errorf("category = %q", definition.Category)
	}
	if definition.RequiresRelay || definition.ReceivesWebhooks {
		t.Error("github is reached outbound under the app credential; no relay, no webhooks")
	}
	if definition.Verify != nil || definition.Probe == nil {
		t.Error("github verifies by probing the live installation, not from gathered facts")
	}
	const wantDescription = "Give investigations read-only access to selected repositories " +
		"for commits, pull requests, CI failures, files, and releases."
	if definition.Description != wantDescription {
		t.Errorf("description = %q, want %q", definition.Description, wantDescription)
	}
}

// The App credential is deployment configuration, so the integration's own configuration
// is the installation id alone — numeric, required, and NOT a secret: it identifies a
// grant, it cannot exercise one.
func TestTheOnlyConfigurationFieldIsTheInstallationID(t *testing.T) {
	t.Parallel()

	definition := Definition(nil, NewClient(""))
	if len(definition.Config) != 1 {
		t.Fatalf("config declares %d fields, want the installation id alone", len(definition.Config))
	}
	field := definition.Config[0]
	if field.Name != "installationId" || field.Type != integrations.FieldInteger ||
		!field.Required || field.Secret {
		t.Errorf("installationId = %+v", field)
	}
}

func TestToolsAndCapabilitiesAgreeOneToOne(t *testing.T) {
	t.Parallel()

	definition := Definition(nil, NewClient(""))

	exercised := map[string]int{}
	for _, tool := range definition.Tools {
		exercised[tool.Capability]++
	}
	for _, capability := range definition.Capabilities {
		if exercised[capability] != 1 {
			t.Errorf("capability %s is exercised by %d tools, want exactly one",
				capability, exercised[capability])
		}
		delete(exercised, capability)
	}
	for capability := range exercised {
		t.Errorf("tool capability %s is not one the definition declares", capability)
	}
}

func TestEveryToolDeclaresItsWholeContract(t *testing.T) {
	t.Parallel()

	for _, tool := range Definition(nil, NewClient("")).Tools {
		if !strings.HasPrefix(tool.Name, "github.") {
			t.Errorf("tool name %q does not carry the provider prefix", tool.Name)
		}
		for field, value := range map[string]string{
			"description":  tool.Description,
			"whenToUse":    tool.WhenToUse,
			"whenNotToUse": tool.WhenNotToUse,
			"permissions":  tool.Permissions,
			"output":       tool.Output,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s declares no %s", tool.Name, field)
			}
		}
		if tool.Run == nil {
			t.Errorf("%s declares no Run", tool.Name)
		}
		for _, argument := range tool.Arguments {
			if argument.Name == "" || argument.Description == "" || argument.Type == "" {
				t.Errorf("%s argument %+v is missing its name, description or type",
					tool.Name, argument)
			}
		}
	}
}

func TestInstallationOfRefusesWhatIsNotAnID(t *testing.T) {
	t.Parallel()

	for name, configuration := range map[string]map[string]any{
		"absent":     {},
		"text":       {"installationId": "77"},
		"fractional": {"installationId": 77.5},
		"negative":   {"installationId": float64(-1)},
	} {
		integration := integrations.Integration{Configuration: configuration}
		if _, err := installationOf(integration); err == nil {
			t.Errorf("an %s installation id was accepted", name)
		}
	}

	integration := integrations.Integration{
		Configuration: map[string]any{"installationId": float64(77)},
	}
	installation, err := installationOf(integration)
	if err != nil || installation != 77 {
		t.Errorf("a whole id was refused: %v %d", err, installation)
	}
}

package integrations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogProjectsConfigurationAndTools(t *testing.T) {
	catalog, err := NewCatalog(Definition{
		Key: "example", Name: "Example", Description: "Reads examples.",
		Category: CategoryCollaboration, RequiresRelay: true,
		Config: []Field{{Key: "token", Label: "Token",
			Type: FieldString, Required: true, Secret: true}},
		Tools: []Tool{{Name: "example_read", Description: "Read one known example. " +
			"Use when its contents are needed. Do not use without a target.", Run: inertTool,
			Arguments: []ToolArgument{{Name: "id", Description: "Example ID", Type: FieldString, Required: true}}}},
		Probe: inertProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := catalog.Lookup("example")
	view := typeViewOf(definition, 0, true)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"logo", "available", "capabilities", "secretFields", "documentationSlug", "documentationUrl"} {
		if _, present := fields[removed]; present {
			t.Errorf("type response still contains %s", removed)
		}
	}
	if !strings.Contains(string(view.ConfigurationSchema), `"writeOnly":true`) {
		t.Error("configuration schema lost the write-only credential")
	}
	view.Tools[0].Name = "changed"
	view.Tools[0].Arguments[0].Name = "changed"
	if definition.Tools[0].Name != "example_read" || definition.Tools[0].Arguments[0].Name != "id" {
		t.Fatal("response projection shares mutable Tool data with the catalog")
	}
}

func inertProbe(_ context.Context, _ ProbeInput) Verification {
	return Verification{Status: StatusVerified}
}

func inertTool(context.Context, ToolRequest) (ToolResult, error) { return ToolResult{}, nil }

func TestCatalogRejectsProviderIncompatibleToolNames(t *testing.T) {
	for _, name := range []string{"example.read", strings.Repeat("x", 65)} {
		_, err := NewCatalog(Definition{
			Key: "example", Name: "Example", Category: CategoryCollaboration,
			Tools: []Tool{{Name: name, Description: "Read an example.", Run: inertTool}},
			Probe: inertProbe,
		})
		if err == nil {
			t.Fatalf("catalog accepted provider-incompatible Tool name %q", name)
		}
	}
}

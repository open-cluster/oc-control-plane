package integrations

import (
	"context"
	"testing"
)

func TestCatalogPublishesProviderManifestsAsItsMetadataSource(t *testing.T) {
	catalog, err := NewCatalog(Definition{
		Manifest: Manifest{
			Key: "example", Name: "Example", Description: "Reads examples.",
			Logo: "example", Category: CategoryCollaboration,
			SourceURL: "https://example.com/docs", RequiresRelay: true,
			DocumentationSlug: "integrations/collaboration/example",
			Config: []Field{{Key: "token", Label: "Token",
				Type: FieldString, Required: true, Secret: true}},
			Tools: []Tool{{Name: "example.read", Description: "Reads an example.",
				WhenToUse: "When an example is needed.", WhenNotToUse: "Without a target.",
				Permissions: "examples:read", Output: "One example.", Run: inertTool}},
		},
		Probe: inertProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifests := catalog.Manifests()
	if len(manifests) != 1 || manifests[0].Key != "example" ||
		manifests[0].Category != CategoryCollaboration ||
		!manifests[0].RequiresRelay ||
		manifests[0].SourceURL != "https://example.com/docs" ||
		manifests[0].DocumentationSlug != "integrations/collaboration/example" ||
		len(manifests[0].Capabilities()) != 1 || manifests[0].Capabilities()[0] != "example.read" ||
		len(manifests[0].SecretFields()) != 1 || manifests[0].SecretFields()[0] != "token" ||
		len(manifests[0].Tools) != 1 || len(manifests[0].ConfigurationSchema()) == 0 {
		t.Fatalf("manifests = %#v", manifests)
	}
	manifests[0].Name = "changed"
	manifests[0].Tools[0].Name = "changed"
	manifests[0].Config[0].Key = "changed"
	if catalog.Manifests()[0].Name != "Example" {
		t.Fatal("manifest caller mutated catalog metadata")
	}
	if catalog.Manifests()[0].Capabilities()[0] != "example.read" ||
		catalog.Manifests()[0].SecretFields()[0] != "token" {
		t.Fatal("manifest caller mutated nested metadata")
	}
	definition, _ := catalog.Lookup("example")
	view := typeViewOf(definition, 0, true)
	if !view.Available || !view.ReceivesWebhooks || view.DocumentationSlug != "integrations/collaboration/example" ||
		view.DocumentationURL != "https://example.com/docs" || len(view.Capabilities) != 1 ||
		len(view.SecretFields) != 1 {
		t.Fatalf("manifest view = %#v", view)
	}
}

func inertProbe(_ context.Context, _ ProbeInput) Verification {
	return Verification{Status: StatusVerified}
}

func inertTool(context.Context, ToolRequest) (ToolResult, error) { return ToolResult{}, nil }

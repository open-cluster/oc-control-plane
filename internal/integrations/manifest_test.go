package integrations

import (
	"context"
	"testing"
)

func TestCatalogPublishesProviderManifestsAsItsMetadataSource(t *testing.T) {
	catalog, err := NewCatalog(Definition{ID: 7, Key: "example", Name: "Example", Description: "Reads examples.", Logo: "example", Category: CategoryCollaboration, Probe: inertProbe})
	if err != nil {
		t.Fatal(err)
	}
	manifests := catalog.Manifests()
	if len(manifests) != 1 || manifests[0].ID != 7 || manifests[0].Key != "example" || manifests[0].Category != CategoryCollaboration {
		t.Fatalf("manifests = %#v", manifests)
	}
	manifests[0].Name = "changed"
	if catalog.Manifests()[0].Name != "Example" {
		t.Fatal("manifest caller mutated catalog metadata")
	}
}

func inertProbe(_ context.Context, _ ProbeInput) Verification {
	return Verification{Status: StatusActive}
}

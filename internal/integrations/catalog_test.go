package integrations

import (
	"context"
	"strings"
	"testing"
)

// Catalog assembly against malformed declarations: a broken declaration fails the
// build's tests, never an investigation.

func assembledWith(argument ToolArgument) error {
	_, err := NewCatalog(Definition{
		Manifest: Manifest{ID: 99, Key: "stub", Name: "Stub", Category: CategoryAlerting,
			Available: true,
			Tools: []Tool{{
				Name: "stub.read", Description: "reads",
				WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
				Output: "data", Arguments: []ToolArgument{argument},
				Run: func(context.Context, ToolRequest) (ToolResult, error) { return ToolResult{}, nil },
			}}},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
	})
	return err
}

func TestCatalogRefusesAMalformedToolArgument(t *testing.T) {
	t.Parallel()

	sound := ToolArgument{Name: "limit", Description: "how many", Type: FieldInteger}
	if err := assembledWith(sound); err != nil {
		t.Fatalf("a complete argument was refused: %v", err)
	}

	cases := map[string]ToolArgument{
		"no name":        {Description: "how many", Type: FieldInteger},
		"no description": {Name: "limit", Type: FieldInteger},
		"no type":        {Name: "limit", Description: "how many"},
		"unknown type":   {Name: "limit", Description: "how many", Type: FieldType("float")},
	}
	for name, argument := range cases {
		if err := assembledWith(argument); err == nil {
			t.Errorf("%s: a malformed declaration must fail assembly", name)
		}
	}
}

func TestCatalogRefusesDuplicateArgumentNames(t *testing.T) {
	t.Parallel()

	_, err := NewCatalog(Definition{
		Manifest: Manifest{ID: 99, Key: "stub", Name: "Stub", Category: CategoryAlerting,
			Available: true,
			Tools: []Tool{{
				Name: "stub.read", Description: "reads",
				WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
				Output: "data",
				Arguments: []ToolArgument{
					{Name: "limit", Description: "how many", Type: FieldInteger},
					{Name: "limit", Description: "again", Type: FieldString},
				},
				Run: func(context.Context, ToolRequest) (ToolResult, error) { return ToolResult{}, nil },
			}}},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a duplicate argument name must fail assembly by name, got %v", err)
	}
}

// OUR DOCUMENTATION PAGE, DERIVED FROM WHAT THE DEFINITION ALREADY KNOWS.
//
// Every type named the vendor's documentation and none named ours, so the page carrying the
// receiver YAML, the header name and the version floor existed, was accurate, and was
// unreachable from the product.

func TestTheProductDocumentationURLIsDerivedFromTheDefinition(t *testing.T) {
	t.Parallel()

	// The path the documentation gate already asserts a page exists at: the role
	// directory is the Category and the page is the Key. Derived rather than declared, so
	// a hand-written URL cannot drift away from the page it names.
	definition := Definition{Manifest: Manifest{DocumentationSlug: "integrations/collaboration/slack"}}
	const want = "https://docs.opencluster.dev/integrations/collaboration/slack"
	if got := definition.ProductDocumentationURL(); got != want {
		t.Errorf("ProductDocumentationURL() = %q, want %q", got, want)
	}
}

func TestAnIncompleteDefinitionNamesNoDocumentationPage(t *testing.T) {
	t.Parallel()

	// A URL composed from a missing half would point at a page that cannot exist, and
	// sending an operator to a 404 is worse than sending them nowhere. Unreachable
	// through the catalog, which refuses an assembly missing either, and answered here so
	// the composition is total.
	for name, definition := range map[string]Definition{
		"empty": {Manifest: Manifest{}},
	} {
		if got := (definition).ProductDocumentationURL(); got != "" {
			t.Errorf("%s composed %q", name, got)
		}
	}
}

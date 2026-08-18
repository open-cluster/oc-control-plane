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
		ID: 99, Key: "stub", Name: "Stub", Category: CategoryAlerting,
		Capabilities: []string{"stub.read"},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
		Tools: []Tool{{
			Name: "stub.read", Capability: "stub.read", Description: "reads",
			WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
			Output:    "data",
			Arguments: []ToolArgument{argument},
			Run: func(context.Context, ToolRequest) (ToolResult, error) {
				return ToolResult{}, nil
			},
		}},
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
		ID: 99, Key: "stub", Name: "Stub", Category: CategoryAlerting,
		Capabilities: []string{"stub.read"},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
		Tools: []Tool{{
			Name: "stub.read", Capability: "stub.read", Description: "reads",
			WhenToUse: "always", WhenNotToUse: "never", Permissions: "none",
			Output: "data",
			Arguments: []ToolArgument{
				{Name: "limit", Description: "how many", Type: FieldInteger},
				{Name: "limit", Description: "again", Type: FieldString},
			},
			Run: func(context.Context, ToolRequest) (ToolResult, error) {
				return ToolResult{}, nil
			},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a duplicate argument name must fail assembly by name, got %v", err)
	}
}

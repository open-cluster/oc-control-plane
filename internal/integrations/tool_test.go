package integrations

import (
	"encoding/json"
	"strings"
	"testing"
)

// The native tool definition: the single declarative Tool contract rendered into what a
// provider's tool-calling API is given. These tests pin that the composed description
// carries every part an investigator routes by — including Output, which used to be
// validated as mandatory and shown to nobody — and that the generated input schema is
// closed and typed.

func definedTool() Tool {
	return Tool{
		Name:        "example.read_things",
		Description: "Reads one thing's records inside a time window.",
		WhenToUse:   "To answer what changed before this broke.",
		WhenNotToUse: "Not for finding the thing; that is example.list_things. " +
			"Never repeatedly inside one investigation.",
		Arguments: []ToolArgument{
			{Name: "thingId", Description: "The thing's stable id.",
				Type: FieldInteger, Required: true},
			{Name: "limit", Description: "How many records, at most 100.",
				Type: FieldInteger},
			{Name: "since", Description: "Start of the window, RFC 3339.",
				Type: FieldString},
		},
		Permissions: "the connected account's read grant",
		Output:      "a bounded list of records, plus a truncated flag when more exist",
	}
}

func TestDefinitionComposesTheModelFacingDescription(t *testing.T) {
	definition := definedTool().Definition()
	if definition.Name != "example.read_things" {
		t.Fatalf("name = %q", definition.Name)
	}
	for _, part := range []string{
		"Reads one thing's records inside a time window.",
		"Use when: To answer what changed before this broke.",
		"Do not use when: Not for finding the thing",
		"Returns: a bounded list of records",
	} {
		if !strings.Contains(definition.Description, part) {
			t.Errorf("the composed description is missing %q:\n%s", part, definition.Description)
		}
	}
}

func TestDefinitionGeneratesAClosedTypedInputSchema(t *testing.T) {
	schema := definedTool().Definition().InputSchema

	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("the schema must be a closed object: %v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 3 {
		t.Fatalf("properties = %v", schema["properties"])
	}
	thing, ok := properties["thingId"].(map[string]any)
	if !ok || thing["type"] != "integer" || thing["description"] != "The thing's stable id." {
		t.Fatalf("thingId property = %v", properties["thingId"])
	}
	since, ok := properties["since"].(map[string]any)
	if !ok || since["type"] != "string" {
		t.Fatalf("since property = %v", properties["since"])
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "thingId" {
		t.Fatalf("required = %v", schema["required"])
	}
}

func TestDefinitionWithoutArgumentsRequiresNothing(t *testing.T) {
	bare := definedTool()
	bare.Arguments = nil
	schema := bare.Definition().InputSchema
	if _, present := schema["required"]; present {
		t.Fatalf("an argumentless tool must not render a required list: %v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("an argumentless tool renders an empty properties object: %v", schema)
	}
}

func TestDefinitionRendersDeterministically(t *testing.T) {
	// The definition set feeds the derived agent revision, so its bytes must not move
	// between renders of the same declaration.
	first, err := json.Marshal(definedTool().Definition())
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		again, err := json.Marshal(definedTool().Definition())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("two renders differ:\n%s\n%s", first, again)
		}
	}
}

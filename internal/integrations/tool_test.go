package integrations

import (
	"encoding/json"
	"testing"
)

func definedTool() Tool {
	return Tool{
		Name: "example.read_things",
		Description: "Read one thing's records inside a time window. Use to answer what " +
			"changed before a failure. Do not use to find the thing; use " +
			"example.list_things first. Results are bounded and report truncation.",
		Arguments: []ToolArgument{
			{Name: "thingId", Description: "The thing's stable id.",
				Type: FieldInteger, Required: true},
			{Name: "limit", Description: "How many records, at most 100.",
				Type: FieldInteger},
			{Name: "since", Description: "Start of the window, RFC 3339.",
				Type: FieldString},
		},
	}
}

func TestDefinitionPassesTheAuthoredDescriptionThrough(t *testing.T) {
	definition := definedTool().Definition()
	if definition.Name != "example.read_things" {
		t.Fatalf("name = %q", definition.Name)
	}
	if definition.Description != definedTool().Description {
		t.Fatalf("description = %q, want the authored description unchanged", definition.Description)
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

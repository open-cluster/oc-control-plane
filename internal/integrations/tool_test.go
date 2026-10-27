package integrations

import (
	"testing"
	"time"
)

func definedTool() Tool {
	return Tool{
		Name: "example_read_things",
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
	if definition.Name != "example_read_things" {
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

func window(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}

func TestClampingNarrowsIntoTheInvestigationWindow(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	from, until := request.ClampWindow(
		window(t, "2026-01-01T00:00:00Z"), window(t, "2026-12-31T00:00:00Z"))

	if !from.Equal(request.WindowFrom) || !until.Equal(request.WindowUntil) {
		t.Errorf("clamped to %v..%v; a wider ask must not widen the read", from, until)
	}
}

func TestAnAskEntirelyAfterTheWindowDoesNotInvertIt(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	from, until := request.ClampWindow(window(t, "2026-09-01T00:00:00Z"), time.Time{})

	if from.After(until) {
		t.Errorf("clamped to %v..%v, which starts after it ends", from, until)
	}
}

func TestAnAskEntirelyBeforeTheWindowDoesNotInvertIt(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	from, until := request.ClampWindow(time.Time{}, window(t, "2026-07-01T00:00:00Z"))

	if from.After(until) {
		t.Errorf("clamped to %v..%v, which starts after it ends", from, until)
	}
}

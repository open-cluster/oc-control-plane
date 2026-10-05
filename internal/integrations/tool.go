package integrations

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

type Tool struct {
	Name                string
	Description         string
	Arguments           []ToolArgument
	RequiredGrants      []string
	SupportsThreadScope bool
	Run                 func(ctx context.Context, request ToolRequest) (ToolResult, error)
}

type ToolArgument struct {
	Name        string
	Description string
	Type        FieldType
	Required    bool
}

type ToolRequest struct {
	InvestigationID uuid.UUID
	Integration     Integration
	Credential      string
	Arguments       map[string]any
	OriginChannel   string
	OriginThread    string
	WindowFrom      time.Time
	WindowUntil     time.Time
}

func (r ToolRequest) ClampWindow(from, until time.Time) (time.Time, time.Time) {
	if !r.WindowFrom.IsZero() && (from.IsZero() || from.Before(r.WindowFrom)) {
		from = r.WindowFrom
	}
	if !r.WindowUntil.IsZero() && (until.IsZero() || until.After(r.WindowUntil)) {
		until = r.WindowUntil
	}
	if !from.IsZero() && !until.IsZero() && from.After(until) {
		from = until
	}
	return from, until
}

type ToolResult struct {
	Content     any
	Truncated   bool
	Summary     string
	Sources     []string
	WindowFrom  time.Time
	WindowUntil time.Time
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]any
}

func (t Tool) Definition() ToolDefinition {
	properties := make(map[string]any, len(t.Arguments))
	var required []string
	for _, argument := range t.Arguments {
		schemaType := "string"
		if argument.Type == FieldInteger {
			schemaType = "integer"
		}
		properties[argument.Name] = map[string]any{
			"type":        schemaType,
			"description": argument.Description,
		}
		if argument.Required {
			required = append(required, argument.Name)
		}
	}
	sort.Strings(required)

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = toAny(required)
	}
	return ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
	}
}

func toAny(values []string) []any {
	anys := make([]any, 0, len(values))
	for _, value := range values {
		anys = append(anys, value)
	}
	return anys
}

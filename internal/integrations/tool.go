package integrations

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Tool struct {
	Name                string
	Description         string
	Arguments           []ToolArgument
	RequiredGrants      []string
	SupportsThreadScope bool
	ReadOnly            bool
	ConcurrencySafe     bool
	Run                 func(ctx context.Context, request ToolRequest) (ToolResult, error)
}

type ToolArgument struct {
	Name        string
	Description string
	Type        FieldType
	Required    bool
}

type ToolRequest struct {
	// The host sets execution ownership separately from provider arguments.
	ExecutionID         uuid.UUID
	ExecutionLeaseToken uuid.UUID `json:"-"`
	ToolRunID           uuid.UUID
	InvestigationID     uuid.UUID
	Integration         Integration
	Credential          string
	Arguments           map[string]any
	OriginChannel       string
	OriginThread        string
	WindowFrom          time.Time
	WindowUntil         time.Time
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

// WithInputEnvelope preserves the Tool's argument schema and supplies purpose and source selection.
func (definition ToolDefinition) WithInputEnvelope(sources []Integration) ToolDefinition {
	fields := map[string]any{
		"purpose": map[string]any{
			"type":        "string",
			"description": "Concise operator-visible reason for this read.",
		},
		"input": definition.InputSchema,
	}
	required := []any{"input"}
	if len(sources) > 1 {
		ids := make([]any, 0, len(sources))
		names := make([]string, 0, len(sources))
		for _, source := range sources {
			ids = append(ids, source.ID.String())
			names = append(names, source.Name+" ("+source.ID.String()+")")
		}
		fields["integrationId"] = map[string]any{
			"type":        "string",
			"enum":        ids,
			"description": "Select the connected source: " + strings.Join(names, "; "),
		}
		required = append(required, "integrationId")
	}
	definition.InputSchema = map[string]any{
		"type":                 "object",
		"properties":           fields,
		"required":             required,
		"additionalProperties": false,
	}
	return definition
}

func toAny(values []string) []any {
	anys := make([]any, 0, len(values))
	for _, value := range values {
		anys = append(anys, value)
	}
	return anys
}

package reasoning

// THE OUTPUT SCHEMAS, AND WHAT THEY MAKE UNSTATEABLE.
//
// The model is not being asked to DO anything; it is being asked to return a document —
// either the reads to perform next, or the findings. What is absent is the point: there
// is no field for a hypothesis, an evidence stance or a coverage judgement, because none
// of that is persisted; and a finding's support is an integer ordinal among the runs that
// actually happened, so a citation cannot name a read nobody made.
//
// The schemas use only what every provider's structured-output mode accepts: types,
// properties, required, enum, items and additionalProperties. Bounds — string lengths,
// array sizes — are deliberately NOT expressed here, because several providers silently
// drop them and a bound that is silently dropped is a bound nobody has. They are enforced
// when the answer is decoded, where they hold for every provider equally.
//
// The one open shape is a call's arguments: tools declare their own, providers differ,
// and every argument is validated again by the tool itself at execution — an undeclared
// one is refused there, never dropped.

// SchemaVersion is the version of the documents below. Bumping it invalidates every
// recording made against the old shape.
const SchemaVersion = "4"

// DecisionSchema is what the reasoner returns while reads are still allowed: calls to
// perform next, or findings when it is done. The tool enumeration is the brief's own, so
// a call cannot name a tool the selected sources do not offer.
func DecisionSchema(tools []string) Schema {
	return Schema{
		Name:    "decision",
		Version: SchemaVersion,
		Document: object(
			properties{
				"calls":    array(callSchema(tools)),
				"findings": array(findingSchema()),
			},
		),
	}
}

// ConclusionSchema is what the reasoner returns when reads are over: findings alone.
func ConclusionSchema() Schema {
	return Schema{
		Name:    "conclusion",
		Version: SchemaVersion,
		Document: object(
			properties{
				"findings": array(findingSchema()),
			},
		),
	}
}

func callSchema(tools []string) map[string]any {
	return object(
		properties{
			"tool": enumField(tools...),
			// The tool's own declared arguments. Open here because each tool declares its
			// own; the tool itself refuses an undeclared or ill-typed one at execution.
			"arguments": openObject(),
		},
	)
}

func findingSchema() map[string]any {
	return object(
		properties{
			// What was established, in plain language an operator can act on.
			"statement": stringField,
			// One-based ordinals among the runs already performed. Required and checked at
			// decode: a finding citing nothing, or citing a read that never ran, is refused
			// rather than stored.
			"sources": array(integerField),
		},
	)
}

// The small JSON Schema vocabulary these documents are built from.

type properties map[string]any

var (
	stringField  = map[string]any{"type": "string"}
	integerField = map[string]any{"type": "integer"}
)

func enumField(values ...string) map[string]any {
	allowed := make([]any, 0, len(values))
	for _, value := range values {
		allowed = append(allowed, value)
	}
	return map[string]any{"type": "string", "enum": allowed}
}

func array(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

// object closes the shape: additionalProperties is false and every property is required,
// so a field a provider invented is refused and a field it omitted is refused too.
func object(fields properties) map[string]any {
	required := make([]any, 0, len(fields))
	for name := range fields {
		required = append(required, name)
	}
	sortStrings(required)
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any(fields),
		"required":             required,
		"additionalProperties": false,
	}
}

// openObject is the one deliberately open shape: a call's arguments.
func openObject() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true}
}

// sortStrings orders the required list so a schema renders byte-identically every time.
// Map iteration order is random in Go, and a schema whose bytes moved between requests
// would invalidate a prompt cache for a reason nobody could see.
func sortStrings(values []any) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0; inner-- {
			left, leftOK := values[inner-1].(string)
			right, rightOK := values[inner].(string)
			if !leftOK || !rightOK || left <= right {
				break
			}
			values[inner-1], values[inner] = values[inner], values[inner-1]
		}
	}
}

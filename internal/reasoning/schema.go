package reasoning

// THE CONCLUSION'S SCHEMA VOCABULARY, AND WHAT IT MAKES UNSTATEABLE.
//
// The conclude tool's input is a document. What is absent is the point: there is no
// field for a hypothesis, an evidence stance or a coverage judgement, because none of
// that is persisted; and a finding's support is an integer ordinal among the runs that
// actually happened, so a citation cannot name a read nobody made.
//
// The documents use only what every provider's structured mode accepts: types,
// properties, required, enum, items and additionalProperties. Bounds — string lengths,
// array sizes — are deliberately NOT expressed here, because several providers silently
// drop them and a bound that is silently dropped is a bound nobody has. They are enforced
// when the answer is decoded, where they hold for every provider equally.

// SchemaVersion is the version of the documents built here. Bumping it invalidates
// every recording made against the old shape.
const SchemaVersion = "4"

// The small JSON Schema vocabulary the documents are built from.

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

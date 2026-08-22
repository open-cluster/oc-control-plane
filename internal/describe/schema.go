package describe

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

// A REQUEST BODY'S SHAPE, REFLECTED FROM THE TYPE THAT DECODES IT.
//
// Every write on this surface decodes with DisallowUnknownFields, so a body carrying a
// field this build does not declare is refused rather than partly applied. That is the
// right behaviour and it was undocumented, which made it a surprise: a client sends what it
// believes the shape is, gets 400, and has no way to learn what the shape actually is
// except by reading Go.
//
// The schema is derived from the decoding type rather than authored beside it. A schema
// somebody writes out is a second copy of the contract, and the failure mode of a second
// copy is not that it is wrong on the day it is written — it is that a field renamed in Go
// leaves it behind, silently, and the document then teaches a client something false.
// Reflecting it means the rename IS the documentation change.
//
// The vocabulary is JSON Schema, matching what the integration configuration already
// serves, so a client has one schema dialect to hold rather than two. No toolchain is
// added: what a request body needs is objects, strings, booleans, numbers, arrays and maps.

// SchemaOf reflects a JSON Schema from a value of the type a handler decodes into.
//
// additionalProperties is false at every level because that is exactly what
// DisallowUnknownFields enforces. Stating anything else would document a tolerance this
// service does not have.
func SchemaOf(example any) map[string]any {
	if example == nil {
		return nil
	}
	return schemaFor(reflect.TypeOf(example), map[reflect.Type]bool{})
}

func schemaFor(kind reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}

	// A time is a string on the wire, not the struct it is in Go. Answered before the
	// struct case, which would otherwise reflect its unexported internals into nothing.
	if kind == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}

	switch kind.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(kind.Elem(), visiting)}
	case reflect.Map:
		// A map is an open object BY DECLARATION, and it is the one place
		// additionalProperties is not false: the handler asked for arbitrary keys, so the
		// schema says so rather than describing a closed object the decoder would accept
		// anything into anyway.
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaFor(kind.Elem(), visiting),
		}
	case reflect.Interface:
		// `any` on a request body is a value this build does not constrain — the
		// integration configuration is the case, and its real schema is the type's own,
		// served per type in the catalog. Saying nothing about it is honest; inventing a
		// constraint would not be.
		return map[string]any{}
	case reflect.Struct:
		return objectSchema(kind, visiting)
	default:
		// Channels, functions and the rest never appear on a decoded body. Answering an
		// unconstrained schema rather than panicking keeps a mistake in one field from
		// taking down the description of every route.
		return map[string]any{}
	}
}

// objectSchema reflects a struct as a closed object, following the json tags the decoder
// itself follows so the document and the decoder cannot disagree about a field's name.
func objectSchema(kind reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	if visiting[kind] {
		// A type that contains itself. Nothing on this surface does today; answering an
		// unconstrained object rather than recursing means adding one is a vaguer document
		// and not a stack overflow at startup.
		return map[string]any{"type": "object"}
	}
	visiting[kind] = true
	defer delete(visiting, kind)

	properties := map[string]any{}
	for index := range kind.NumField() {
		field := kind.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			// Unexported: the decoder cannot fill it, so a client cannot send it.
			//
			// An anonymous field is exempt even when its TYPE is unexported, because
			// encoding/json still promotes that type's exported fields — a rule worth
			// following exactly, since the whole value of this schema is that it says what
			// the decoder does rather than what would be reasonable.
			continue
		}
		name, embedded, skip := jsonName(field)
		if skip {
			continue
		}
		if embedded {
			// An embedded struct with no name of its own contributes ITS fields at this
			// level, which is what the decoder does with it.
			for key, value := range objectProperties(field.Type, visiting) {
				properties[key] = value
			}
			continue
		}
		properties[name] = schemaFor(field.Type, visiting)
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
}

func objectProperties(kind reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	if kind.Kind() != reflect.Struct {
		return nil
	}
	nested, _ := objectSchema(kind, visiting)["properties"].(map[string]any)
	return nested
}

// jsonName resolves what a field is called on the wire, by the same rules encoding/json
// applies: the tag's name wins, "-" means the field is not on the wire at all, and an
// embedded struct with no tag contributes its own fields at this level.
func jsonName(field reflect.StructField) (name string, embedded bool, skip bool) {
	tag, tagged := field.Tag.Lookup("json")
	if tagged {
		tag, _, _ = strings.Cut(tag, ",")
		if tag == "-" {
			return "", false, true
		}
		if tag != "" {
			return tag, false, false
		}
	}
	if field.Anonymous {
		return "", true, false
	}
	return field.Name, false, false
}

// Fields reports the wire names one schema declares, sorted. It exists for the gate and the
// tests that hold the document against what the decoder accepts, so neither has to reach
// into the schema's shape and both break together if the shape changes.
func Fields(schema map[string]any) []string {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

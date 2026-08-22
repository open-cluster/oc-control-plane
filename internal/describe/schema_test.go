package describe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// THE SCHEMA MUST REFUSE WHAT THE DECODER REFUSES, AND ACCEPT WHAT IT ACCEPTS.
//
// A published schema that is merely plausible is worse than none: a client believes it,
// builds against it, and meets a 400 it now has no way to explain. The properties asserted
// here are the ones that make the document trustworthy rather than decorative — the field
// names are the ones encoding/json actually reads, and the closedness is the one
// DisallowUnknownFields actually enforces.

type sample struct {
	Name    string            `json:"name"`
	Count   int               `json:"count"`
	Enabled bool              `json:"enabled"`
	Labels  map[string]string `json:"labels"`
	Tags    []string          `json:"tags"`
	Free    map[string]any    `json:"free"`
	At      time.Time         `json:"at"`
	Nested  struct {
		Reason string `json:"reason"`
	} `json:"nested"`
	Renamed string `json:"wireName"`
	Omitted string `json:"-"`
	hidden  string
}

func TestASchemaNamesTheFieldsTheDecoderReads(t *testing.T) {
	t.Parallel()

	// Exactly what encoding/json would accept: the tag's name where there is one, the
	// field's name where there is not, and nothing for a field the wire cannot reach.
	got := Fields(SchemaOf(sample{}))
	want := []string{
		"at", "count", "enabled", "free", "labels", "name", "nested", "tags", "wireName",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
}

func TestTheSchemaAndTheDecoderAgreeOnEveryFieldName(t *testing.T) {
	t.Parallel()

	// The property that matters, asserted mechanically rather than by reading the list
	// above: every field the document publishes is one the decoder accepts, and a field
	// the document does NOT publish is one the decoder refuses. Both directions, because
	// each is a different lie to tell a client.
	published := map[string]bool{}
	for _, name := range Fields(SchemaOf(sample{})) {
		published[name] = true
		if err := decodeStrictly(name); err != nil {
			t.Errorf("the schema publishes %q and the decoder refuses it: %v", name, err)
		}
	}
	for _, absent := range []string{"hidden", "Omitted", "nope"} {
		if published[absent] {
			continue
		}
		if err := decodeStrictly(absent); err == nil {
			t.Errorf("the schema omits %q and the decoder accepts it", absent)
		}
	}
}

// decodeStrictly reports what the real decoder does with a body carrying one named field,
// under exactly the setting every write on this surface uses.
func decodeStrictly(field string) error {
	body, err := json.Marshal(map[string]any{field: nil})
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&sample{})
}

func TestAnObjectIsClosedBecauseTheDecoderClosesIt(t *testing.T) {
	t.Parallel()

	schema := SchemaOf(sample{})
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v; every write refuses a field it does not "+
			"declare, and a schema saying otherwise documents a tolerance this service "+
			"does not have", schema["additionalProperties"])
	}

	// And the same at depth. A nested object that quietly allowed extra fields would be a
	// client sending one and being refused for a reason the document denied.
	properties, _ := schema["properties"].(map[string]any)
	nested, _ := properties["nested"].(map[string]any)
	if nested["additionalProperties"] != false {
		t.Errorf("a nested object is open: %+v", nested)
	}
}

func TestAMapIsTheOnePlaceExtraKeysAreAllowed(t *testing.T) {
	t.Parallel()

	// A map is an open object BY DECLARATION — the handler asked for arbitrary keys — so
	// the schema says so rather than describing a closed object the decoder would fill
	// with anything anyway.
	properties, _ := SchemaOf(sample{})["properties"].(map[string]any)
	labels, _ := properties["labels"].(map[string]any)
	if labels["type"] != "object" {
		t.Fatalf("a map is described as %v", labels["type"])
	}
	values, _ := labels["additionalProperties"].(map[string]any)
	if values["type"] != "string" {
		t.Errorf("a map of strings describes its values as %+v", labels["additionalProperties"])
	}
}

func TestATimeIsAStringOnTheWire(t *testing.T) {
	t.Parallel()

	// Not the struct it is in Go. Reflecting its unexported internals would publish an
	// empty closed object, which would tell a client a timestamp may carry no fields.
	properties, _ := SchemaOf(sample{})["properties"].(map[string]any)
	at, _ := properties["at"].(map[string]any)
	if at["type"] != "string" || at["format"] != "date-time" {
		t.Errorf("a time is described as %+v", at)
	}
}

func TestAnUnconstrainedValueSaysNothingRatherThanGuessing(t *testing.T) {
	t.Parallel()

	// `any` is a value this build does not constrain — an integration's configuration is
	// the case, and its real schema is the type's own, served per type in the catalog.
	// Inventing a constraint here would contradict it.
	properties, _ := SchemaOf(sample{})["properties"].(map[string]any)
	free, _ := properties["free"].(map[string]any)
	values, _ := free["additionalProperties"].(map[string]any)
	if len(values) != 0 {
		t.Errorf("an unconstrained value is described as %+v", values)
	}
}

func TestASchemaSurvivesAPointerAndAnEmbeddedStruct(t *testing.T) {
	t.Parallel()

	type inner struct {
		Shared string `json:"shared"`
	}
	type outer struct {
		inner
		Own *string `json:"own"`
	}

	// An embedded struct with no name of its own contributes ITS fields at this level,
	// which is what the decoder does with it, and a pointer describes what it points at.
	got := Fields(SchemaOf(outer{}))
	if !reflect.DeepEqual(got, []string{"own", "shared"}) {
		t.Errorf("fields = %v, want the embedded field flattened beside the pointer", got)
	}
}

func TestNothingIsDescribedForNothing(t *testing.T) {
	t.Parallel()

	if schema := SchemaOf(nil); schema != nil {
		t.Errorf("a route with no body described one: %+v", schema)
	}
}

package reasoning

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Rendering a run into the conversation: arguments on one line, content bounded with
// the cut said out loud, timestamps in one spelling.

// maxRunContentBytes bounds how much of one run's content reaches the prompt. A bounded
// read already keeps real contents small; this is the ceiling that keeps a pathological
// one from consuming the context window.
const maxRunContentBytes = 16 << 10

// compactArguments renders a call's scope on one line. Marshalling failure cannot happen
// for a map that itself arrived as JSON; the fallback keeps the record honest anyway.
func compactArguments(arguments map[string]any) string {
	if len(arguments) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return fmt.Sprintf("%v", arguments)
	}
	return string(encoded)
}

// boundedJSON renders a run's content inside the per-run ceiling, saying so when it cut.
//
// List content is cut BETWEEN elements: whole items render until the budget, and the
// note says how many of how many survived — the model reads valid records plus an
// honest count, never JSON severed mid-token. Non-list content falls back to a byte
// cut, repaired to valid UTF-8: json.Marshal leaves multi-byte text unescaped, and a
// rune split at the byte boundary would hand the provider bytes it may refuse.
func boundedJSON(content any) string {
	if content == nil {
		return "null"
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return `"the content could not be rendered"`
	}
	if len(encoded) <= maxRunContentBytes {
		return string(encoded)
	}
	if elements := listElements(content); elements != nil {
		if rendered, kept := boundedList(elements); kept > 0 {
			return rendered
		}
		// The first element alone exceeds the budget; a byte cut of it beats an
		// empty list.
	}
	return strings.ToValidUTF8(string(encoded[:maxRunContentBytes]), "") +
		"… [cut at " + strconv.Itoa(maxRunContentBytes) + " bytes]"
}

// listElements reads content as a list when it is one, whatever its element type.
func listElements(content any) []any {
	value := reflect.ValueOf(content)
	if value.Kind() != reflect.Slice {
		return nil
	}
	elements := make([]any, value.Len())
	for index := range elements {
		elements[index] = value.Index(index).Interface()
	}
	return elements
}

// boundedList renders whole elements until the budget and counts the rest, reporting
// how many it kept so a list whose very first element bursts the budget can fall back
// to a byte cut instead of rendering as empty.
func boundedList(elements []any) (string, int) {
	var rendered strings.Builder
	rendered.WriteString("[")
	kept := 0
	for _, element := range elements {
		encoded, err := json.Marshal(element)
		if err != nil {
			break
		}
		if rendered.Len()+len(encoded)+1 > maxRunContentBytes {
			break
		}
		if kept > 0 {
			rendered.WriteString(",")
		}
		rendered.Write(encoded)
		kept++
	}
	rendered.WriteString("]")
	if kept < len(elements) {
		rendered.WriteString(" … [" + strconv.Itoa(kept) + " of " +
			strconv.Itoa(len(elements)) + " items; the rest cut at " +
			strconv.Itoa(maxRunContentBytes) + " bytes]")
	}
	return rendered.String(), kept
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

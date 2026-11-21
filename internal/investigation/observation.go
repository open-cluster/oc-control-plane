package investigation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

func boundText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func toolRedactor(credentials []string) *strings.Replacer {
	var secrets []string
	for _, credential := range credentials {
		if credential == "" {
			continue
		}
		secrets = append(secrets, credential)
		encoded, _ := json.Marshal(credential)
		secrets = append(secrets, string(encoded[1:len(encoded)-1]))
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	pairs := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		pairs = append(pairs, secret, "[redacted]")
	}
	return strings.NewReplacer(pairs...)
}

// RedactToolInput prepares the Tool Run input for durable admission.
func RedactToolInput(run *ToolRun, credentials ...string) {
	redactToolInput(run, toolRedactor(credentials))
}

func redactToolInput(run *ToolRun, replace *strings.Replacer) {
	var argument func(any) any
	argument = func(value any) any {
		switch typed := value.(type) {
		case map[string]any:
			safe := make(map[string]any, len(typed))
			for key, value := range typed {
				if !audit.NamesACredential(key) {
					safe[replace.Replace(key)] = argument(value)
				}
			}
			return safe
		case []any:
			safe := make([]any, len(typed))
			for index, value := range typed {
				safe[index] = argument(value)
			}
			return safe
		case string:
			return replace.Replace(typed)
		default:
			return value
		}
	}
	if run.Arguments != nil {
		run.Arguments = argument(run.Arguments).(map[string]any)
	}
	run.Tool = boundText(replace.Replace(run.Tool), 128)
	run.Purpose = boundText(replace.Replace(run.Purpose), 512)
}

// PrepareToolObservation retains bounded, sanitized evidence for the model and the store.
func PrepareToolObservation(run *ToolRun, credentials ...string) {
	replace := toolRedactor(credentials)
	redactToolInput(run, replace)
	run.Summary = boundText(replace.Replace(run.Summary), 512)
	run.Error = boundText(replace.Replace(run.Error), 1024)
	if len(credentials) > 0 {
		run.Sources = append([]string(nil), run.Sources...)
		for index, source := range run.Sources {
			run.Sources[index] = replace.Replace(source)
		}
	}
	if run.WindowApplied == nil {
		run.WindowApplied = new(bool)
	}
	// string building ----->
	out := &strings.Builder{}
	header := "[run " + strconv.Itoa(run.Ordinal) + "]"
	if run.Ordinal == 0 {
		header = "[Tool Run " + run.ID.String() + "]"
	}
	out.WriteString(header + " " +
		run.Tool + " " + compactArguments(run.Arguments) + "\n")
	if run.Outcome == RunFailed {
		out.WriteString("FAILED: " + run.Error + "\n")
	} else {
		if *run.WindowApplied {
			out.WriteString("WINDOW: effective bounds " + run.WindowFrom.UTC().Format(time.RFC3339) + " to " +
				run.WindowUntil.UTC().Format(time.RFC3339) + "\n")
		}
		if run.Summary != "" {
			out.WriteString("SUMMARY: " + run.Summary + "\n")
		}
		if run.Truncated {
			out.WriteString("SOURCE: this result may be incomplete; additional records are not proven.\n")
		}
		content, clipped := boundedJSON(run.Content, replace)
		run.ModelClipped = clipped
		out.WriteString("CONTENT: " + content + "\n")
	}
	text := out.String()
	if len(text) > MaxObservationBytes {
		run.Arguments = nil
		run.ModelClipped = true
		text = fmt.Sprintf("%s %s {}\nOBSERVATION UNAVAILABLE: prepared result exceeded %d bytes; arguments and content were omitted.\n",
			header, run.Tool, MaxObservationBytes)
	}
	run.ObservationText = &text
	run.Content = nil
}

const MaxToolContentBytes = 16 << 10

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

func boundedJSON(content any, replace *strings.Replacer) (string, bool) {
	if content == nil {
		return "null", false
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return `"the content could not be rendered"`, true
	}
	text := replace.Replace(string(encoded))
	if len(text) <= MaxToolContentBytes {
		return text, false
	}
	if elements := listElements(content); elements != nil {
		if rendered, kept := boundedList(elements, replace); kept > 0 {
			return rendered, true
		}
	}
	return strings.ToValidUTF8(text[:MaxToolContentBytes], "") +
		"… [cut at " + strconv.Itoa(MaxToolContentBytes) + " bytes]", true
}

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

func boundedList(elements []any, replace *strings.Replacer) (string, int) {
	var rendered strings.Builder
	rendered.WriteString("[")
	kept := 0
	for _, element := range elements {
		encoded, err := json.Marshal(element)
		if err != nil {
			break
		}
		text := replace.Replace(string(encoded))
		punctuation := 1
		if kept > 0 {
			punctuation++
		}
		if rendered.Len()+len(text)+punctuation > MaxToolContentBytes {
			break
		}
		if kept > 0 {
			rendered.WriteString(",")
		}
		rendered.WriteString(text)
		kept++
	}
	rendered.WriteString("]")
	if kept < len(elements) {
		rendered.WriteString(" … [" + strconv.Itoa(kept) + " of " +
			strconv.Itoa(len(elements)) + " items; the rest cut at " +
			strconv.Itoa(MaxToolContentBytes) + " bytes]")
	}
	return rendered.String(), kept
}

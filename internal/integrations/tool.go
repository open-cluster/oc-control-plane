package integrations

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Tool is one bounded read a provider offers an investigation.
//
// The metadata is not documentation garnish: it is what anything routing between tools
// reads, so a tool that cannot say when NOT to use it is a tool that gets used wrongly
// and then patched with prompts. Every field except the argument list is required at
// catalog assembly.
type Tool struct {
	// Name identifies the tool: "slack.get_channel_history". Stable, because provenance
	// records carry it.
	Name string
	// Description says what the tool does, in one or two sentences.
	Description string
	// WhenToUse says which questions this tool answers.
	WhenToUse string
	// WhenNotToUse names the misuses: the questions a caller will be tempted to bring
	// here that belong to another tool or nowhere.
	WhenNotToUse string
	// Arguments is what a call may carry. An argument nothing declares is refused by the
	// tool, never dropped.
	Arguments []ToolArgument
	// Permissions names what the connected account must be granted for this tool to work.
	// An operator and audit consumer only: it never reaches the model, which routes by
	// the composed description instead.
	Permissions string
	// Requires names the verified grants an Integration must have recorded for this
	// tool to be OFFERED to an investigation at all. Empty means always offered. It is
	// how availability derives from verified reality: a tool whose path cannot work
	// with the connected credential — a bot token asked to run user-token search — is
	// absent from the set, never a call that always fails.
	Requires []string
	// ConversationScoped marks a read that can be restricted to the provider thread
	// that originated a Conversation. Broader reads are never implicitly granted by
	// mentioning the application in that thread.
	ConversationScoped bool
	// Output says what a successful answer holds. Composed into the model-facing
	// description ("Returns: …") and rendered on the operator catalog view.
	Output string
	// Run performs the read, bounded by the declared arguments and the request's context.
	Run func(ctx context.Context, request ToolRequest) (ToolResult, error)
}

// ToolArgument is one declared input.
type ToolArgument struct {
	Name        string
	Description string
	Type        FieldType
	Required    bool
}

// ToolRequest is one call. The credential is the unsealed outbound credential, present
// only for the duration of the call and never stored by anything downstream of it.
type ToolRequest struct {
	// InvestigationID attaches durable execution to the Investigation that owns it.
	// Direct provider reads outside an Investigation leave the identifier empty.
	InvestigationID uuid.UUID
	Integration     Integration
	Credential      string
	Arguments       map[string]any
	// OriginChannel and OriginThread structurally constrain a provider-originated
	// Conversation read. Empty values mean this Investigation has no provider thread.
	OriginChannel string
	OriginThread  string
	// WindowFrom and WindowUntil are the investigation's own time bounds. A tool whose
	// arguments carry a window clamps them inside this one, so a read cannot leave the
	// investigation's scope however its arguments were phrased. Zero means unbounded —
	// a direct call outside any investigation.
	WindowFrom  time.Time
	WindowUntil time.Time
}

// ClampWindow narrows an argument window into this request's own. A read phrased with a
// wider window still runs inside the investigation's: the bound is structural, not
// something a prompt asked for. Zero request bounds — a direct call outside any
// investigation — clamp nothing.
func (r ToolRequest) ClampWindow(from, until time.Time) (time.Time, time.Time) {
	if !r.WindowFrom.IsZero() && (from.IsZero() || from.Before(r.WindowFrom)) {
		from = r.WindowFrom
	}
	if !r.WindowUntil.IsZero() && (until.IsZero() || until.After(r.WindowUntil)) {
		until = r.WindowUntil
	}
	// An ask lying entirely outside the investigation's window intersects it in nothing,
	// and the two clamps above express that as a window running backwards. Empty is the
	// honest answer and a backwards window is not: it reads as nonsense to the vendor
	// being queried, to the run record, and to the model now shown the window it got.
	if !from.IsZero() && !until.IsZero() && from.After(until) {
		from = until
	}
	return from, until
}

// ToolResult is one answer.
type ToolResult struct {
	// Content is the answer in types that marshal to JSON; its shape is the tool's Output
	// promise kept.
	Content any
	// Truncated reports that the source held more than the bound, so a reader cannot
	// mistake a page for the whole.
	Truncated bool
	// Summary is the tool's own one-line account of what came back, for the provenance
	// record; Sources are the identifiers of what was read — channel ids, repository
	// ids — so a finding built on this run can be followed to its origin.
	Summary string
	Sources []string
	// WindowFrom and WindowUntil are the window this read ACTUALLY covered, which is not
	// what was asked for whenever ClampWindow narrowed it — including the case of a read
	// phrased with no window at all, which is silently the investigation's own. A read
	// that covers no window leaves them zero and claims none. Without this an empty
	// answer reads as a fact about the estate rather than about the bounds it was given.
	WindowFrom  time.Time
	WindowUntil time.Time
}

// ToolDefinition is the model-facing rendering of one Tool: the single declarative
// contract turned into what a provider's native tool-calling API is given. Nothing else
// may hand-write a second representation of a tool — a provider adapter translates THIS
// value into its vendor's wire shape.
type ToolDefinition struct {
	Name string
	// Description is composed of the declared contract: what the tool does, when to
	// use it, when not to, and what comes back — the whole of what a model routes by,
	// in one place.
	Description string
	// InputSchema is JSON Schema for the call's arguments, generated from the declared
	// argument list: typed, described, required listed, closed to inventions. Rendered
	// deterministically so provider prompt caches remain stable.
	InputSchema map[string]any
}

// Definition renders this tool's model-facing definition.
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
		Name: t.Name,
		Description: t.Description +
			"\nUse when: " + t.WhenToUse +
			"\nDo not use when: " + t.WhenNotToUse +
			"\nReturns: " + t.Output,
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

package reasoning

import (
	"context"
	"encoding/json"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// THE PROVIDER CONTRACT, AND THE REASON IT MENTIONS NO VENDOR.
//
// Which vendor answers is an operational fact, not an architectural one: a model is a deployment
// choice that moves with price, availability, regional obligation and what a tenant will consent
// to. Every one of those becomes an engineering project if a vendor's vocabulary reaches the types
// above it, so nothing here is a message-role array, a vendor stop-reason string, a vendor usage
// shape or an SDK type. An adapter translates in both directions and is the only component on
// either side that knows both vocabularies.

// Provider is one vendor's deployment, reduced to what reasoning actually asks of it.
//
// It is deliberately small. A provider does not hold a conversation, manage an investigation,
// decide when to stop or interpret evidence: it is handed a rendered prompt and a declared output
// schema, and returns a document. Everything else in this package is the same for every vendor,
// which is what keeps a second adapter to one directory.
type Provider interface {
	// Name identifies the vendor in configuration and telemetry. It is stable and
	// lower-case: it is written into records that outlive this build.
	Name() string
	// Complete asks for one document.
	//
	// The returned Completion is populated even when the error is non-nil, because a refused or
	// truncated request still consumed tokens, still names a model and still carries a request
	// identifier — and all three have to reach the spend record. A caller reads the Completion
	// for the figures and the error for the outcome.
	Complete(ctx context.Context, prompt Prompt) (Completion, error)
}

// Prompt is one rendered ask, in the shape every provider is given it.
//
// The block ordering is a design rule rather than a preference. Caching is a prefix match, so
// nothing volatile — no timestamp, no identifier, no per-request value — may appear before the
// last block marked cacheable, or the cache silently stops working and looks exactly like a cache
// that is working.
type Prompt struct {
	// Model is the exact deployment identifier to ask. It is never constructed by appending a
	// suffix to a family name: a constructed identifier is a 404 at best.
	Model string
	// System is the frozen preamble. It is identical across every investigation in every
	// organization, which is what makes it worth caching once and reading from everything.
	System []Block
	// Content is the rendered deliberation, in the order the ordinals in the answer refer to.
	Content []Block
	// Schema is the output contract. The model is not being asked to do anything; it is being
	// asked to return a document, and this is the shape of the document. Empty on a
	// native-tool-calling prompt, where the conclude tool's input schema is the contract.
	Schema Schema
	// Tools are the native tool definitions, generated from the one declarative
	// contract — never a second hand-written representation. Present, they put the
	// prompt in tool-calling mode: the adapter translates each into its vendor's wire
	// shape, and the answer may carry tool calls.
	Tools []integrations.ToolDefinition
	// ForceTool names the one tool the answer must call — the forced concluding turn.
	// Empty leaves the choice to the model.
	ForceTool string
	// Turns is the conversation so far, oldest first: each the assistant's own prior
	// move with what answered it. The adapter replays them verbatim, because caching is
	// a prefix match and the transcript is the prefix.
	Turns []Turn
	// MaxOutputTokens bounds the answer. On providers where thinking and answer text share the
	// bound, a value sized around the answer alone truncates mid-thought.
	MaxOutputTokens int64
	// Effort is how hard to think. It is the primary cost and latency lever and the right value
	// is an empirical question, so it is configuration rather than a constant.
	Effort Effort
}

// Turn is one completed exchange in a tool-calling conversation: what the assistant
// said and asked for, then what answered it.
type Turn struct {
	Assistant AssistantTurn
	// Results answer the assistant's calls, in the same order.
	Results []ToolResultTurn
	// Instruction is trailing user text after the results — the forced-conclusion
	// reason, rendered for the model to act on. Usually empty.
	Instruction string
}

// AssistantTurn is the model's own prior move, echoed back on later requests.
type AssistantTurn struct {
	Text  string
	Calls []CompletionCall
	// Raw is the producing adapter's own verbatim rendering of this turn, opaque to
	// everything else. An adapter whose vendor requires the turn replayed exactly — a
	// thinking block with its signature, say — stores it here and prefers it when
	// rebuilding; adapters that can rebuild from the neutral fields ignore it.
	Raw []byte
}

// CompletionCall is one native tool call, in this system's shape.
type CompletionCall struct {
	// ID is the provider's own identifier for the call, echoed back with its result.
	ID   string
	Name string
	// Arguments is the call's input exactly as the model produced it.
	Arguments json.RawMessage
}

// ToolResultTurn is one call's answer, paired by the call's own identifier.
type ToolResultTurn struct {
	CallID  string
	Content string
	// IsError marks a read that failed or was refused, so the model treats the content
	// as the reason rather than as data.
	IsError bool
}

// Block is one span of prompt text and whether a cacheable prefix ends at it.
//
// Cache is structural rather than a vendor marker: it says this much of the prompt is stable, and
// what a provider does with that — a breakpoint, an automatic prefix, nothing at all — is the
// adapter's problem.
type Block struct {
	Text  string
	Cache bool
}

// Schema is the declared output contract, as one JSON Schema this repository owns.
//
// It is provider-neutral on purpose. A vendor-specific schema dialect would put the output
// contract inside an adapter, where a second vendor could quietly diverge from it — and the
// contract is exactly the thing that must not differ by who answered.
type Schema struct {
	// Name identifies the schema to a provider that wants one.
	Name string
	// Version is bumped when the shape changes, so anything keyed on the schema — a test
	// fixture, a recorded answer — cannot silently replay against a different shape.
	Version string
	// Document is the JSON Schema itself.
	Document map[string]any
}

// Completion is what one provider returned.
type Completion struct {
	// Model is the model that ANSWERED, read from the response rather than echoed from the
	// request: a provider may re-serve a request on another model, and the record must
	// name what actually spoke.
	Model string
	// RequestID is the provider's own identifier for this call, which is what a vendor support
	// conversation is conducted in.
	RequestID string
	// Document is the answer, as the bytes the schema describes. On a tool-calling
	// prompt it is any plain answer text instead.
	Document []byte
	// ToolCalls are the native calls the answer asked for, empty when it did not.
	ToolCalls []CompletionCall
	// Raw is the adapter's own verbatim rendering of this assistant turn, for replay —
	// see AssistantTurn.Raw.
	Raw []byte
	// Stop is why generation ended, normalized. It is read before the document is, because
	// reading the document first is the defect that presents a refusal as a conclusion.
	Stop Stop
	// Usage is what the call consumed, with unreported fields absent rather than zero.
	Usage TokenUsage
}

// Stop is why a provider stopped generating, in this system's terms.
//
// The values are recorded, so they are named rather than inherited from any vendor's spelling.
type Stop int16

const (
	// StopComplete is a finished answer.
	StopComplete Stop = iota + 1
	// StopRefused is the provider's own safeguards declining. It is a successful response
	// carrying a refusal, not a transport error, which is why it has to be checked for
	// explicitly.
	StopRefused
	// StopTruncated is the output ceiling being reached before the answer finished. The document
	// is incomplete and cannot be trusted to parse.
	StopTruncated
	// StopToolUse is a turn that ended by asking for tools: the completion carries the
	// calls, and the conversation continues with their results.
	StopToolUse
)

func (s Stop) String() string {
	switch s {
	case StopComplete:
		return "complete"
	case StopRefused:
		return "refused"
	case StopTruncated:
		return "truncated"
	case StopToolUse:
		return "tool_use"
	default:
		return "unrecognised"
	}
}

// Effort is how hard a provider should think before answering.
//
// The vocabulary is this system's and each adapter maps it onto whatever its vendor accepts,
// including mapping several of these onto one where a vendor offers fewer levels. A provider that
// has no such control ignores it.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	// EffortExtraHigh sits between high and max on vendors that offer it.
	EffortExtraHigh Effort = "xhigh"
	EffortMax       Effort = "max"
)

// Valid reports whether this is an effort level this system recognises. An unrecognised level is
// refused at startup rather than sent to a provider that would reject it mid-round.
func (e Effort) Valid() bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortExtraHigh, EffortMax:
		return true
	default:
		return false
	}
}

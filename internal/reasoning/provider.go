package reasoning

import "context"

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
	// asked to return a document, and this is the shape of the document.
	Schema Schema
	// MaxOutputTokens bounds the answer. On providers where thinking and answer text share the
	// bound, a value sized around the answer alone truncates mid-thought.
	MaxOutputTokens int64
	// Effort is how hard to think. It is the primary cost and latency lever and the right value
	// is an empirical question, so it is configuration rather than a constant.
	Effort Effort
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
	// Document is the answer, as the bytes the schema describes.
	Document []byte
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
)

func (s Stop) String() string {
	switch s {
	case StopComplete:
		return "complete"
	case StopRefused:
		return "refused"
	case StopTruncated:
		return "truncated"
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

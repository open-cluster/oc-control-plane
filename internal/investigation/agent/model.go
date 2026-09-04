package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// Which vendor and which model answer, as configuration rather than as constants.
//
// A model is a deployment choice that moves with availability and regional obligations. Keeping
// it in configuration avoids compiling an operational choice into a release.

// Secret is a credential that must never be rendered.
//
// String, GoString and the JSON form are all the same fixed placeholder, so a credential cannot
// reach a log line, an error message or a case file by being interpolated somewhere nobody thought
// about. Reading the real value is an explicit call, which makes the handful of places that do it
// greppable.
type Secret string

// redacted is what a credential looks like everywhere except the one call that reveals it.
const redacted = "[redacted]"

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return `"` + redacted + `"` }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) LogValue() any                { return redacted }
func (s Secret) Reveal() string             { return string(s) }
func (s Secret) Empty() bool                { return strings.TrimSpace(string(s)) == "" }

// Deployment is one configured provider, model and the bounds it runs under.
type Deployment struct {
	// Provider names the adapter that serves this deployment.
	Provider string
	// Model is the exact identifier, complete as written. No suffix is appended: a constructed
	// identifier is a 404 at best and a different model at worst.
	Model string
	// Effort is how hard to think, the primary resource and latency lever.
	Effort Effort
	// MaxOutputTokens bounds one answer. It is set generously where thinking and answer share the
	// bound, because a value sized around the answer alone truncates mid-thought.
	MaxOutputTokens int64
	// BaseURL overrides where the provider is reached. It is also the ONLY host this deployment
	// may reach: the allowed host is derived from configuration rather than from anything a
	// response contains, so a redirect cannot move where the credential is sent.
	BaseURL string
	// Credential is the API key, read from a file path and never from an environment value.
	Credential Secret
	// RequestTimeout bounds one call. Requests are retried, so the wall clock a single call can
	// consume is this multiplied by the attempts allowed; the product must still fit inside the
	// round's deadline.
	RequestTimeout time.Duration
	// MaxAttempts is how many times one call may be tried before the outcome is an outage.
	MaxAttempts int
}

// Bounds that are safe rather than ambitious. A deployment that names none of them gets these,
// and every one of them is a restriction rather than a target.
const (
	defaultMaxOutputTokens = 32_000
	// Sized by measurement rather than by taste. A model that thinks before answering has been
	// observed taking over two minutes on a single conclusion, and a timeout that fires on a
	// provider which is working reports an outage that never happened — which sends someone to
	// look at a healthy vendor while the real answer was one minute away.
	defaultRequestTimeout = 5 * time.Minute
	defaultMaxAttempts    = 3
)

// WithDefaults fills what an operator did not name. It never loosens what they did.
func (d Deployment) WithDefaults() Deployment {
	if d.Effort == "" {
		d.Effort = EffortHigh
	}
	if d.MaxOutputTokens <= 0 {
		d.MaxOutputTokens = defaultMaxOutputTokens
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = defaultRequestTimeout
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = defaultMaxAttempts
	}
	return d
}

// Validate refuses a deployment that could not work, at startup, where the person who chose the
// values is still the person reading the error. The alternative is discovering it on the first
// round at 03:00, by which time nobody remembers configuring it.
func (d Deployment) Validate() error {
	switch {
	case strings.TrimSpace(d.Provider) == "":
		return fmt.Errorf("a model deployment must name a provider")
	case strings.TrimSpace(d.Model) == "":
		return fmt.Errorf("the %s deployment must name an exact model identifier", d.Provider)
	case !d.Effort.Valid():
		return fmt.Errorf("the %s deployment names effort %q, which is not one of low, medium, "+
			"high, xhigh or max", d.Provider, d.Effort)
	case d.Credential.Empty():
		return fmt.Errorf("the %s deployment has no credential; it is read from a file path so "+
			"that it cannot leak through a process listing", d.Provider)
	}
	if d.BaseURL != "" {
		parsed, err := url.Parse(d.BaseURL)
		loopback := false
		if err == nil {
			address := net.ParseIP(parsed.Hostname())
			loopback = parsed.Hostname() == "localhost" || address != nil && address.IsLoopback()
		}
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback)) {
			return fmt.Errorf(
				"the %s deployment names a base url that is not an https host or local "+
					"loopback; the adapter may reach that host and nothing else", d.Provider)
		}
	}
	return nil
}

// String renders a deployment for a log line. The credential is a Secret, so it cannot appear
// here even if this method is later changed to print the whole struct.
func (d Deployment) String() string {
	return fmt.Sprintf("%s/%s effort=%s max_output=%d", d.Provider, d.Model, d.Effort,
		d.MaxOutputTokens)
}

// Model performs one provider completion.
//
// It is deliberately small. A provider does not hold a conversation, manage an investigation,
// decide when to stop or interpret evidence: it is handed a rendered prompt and a declared output
// schema, and returns a document. Everything else in this package is the same for every vendor,
// which is what keeps a second adapter to one directory.
type Model interface {
	// Complete asks for one document.
	//
	// The returned Completion is populated even when the error is non-nil, because a refused or
	// truncated request still consumed tokens, still names a model and still carries a request
	// identifier — and all three have to reach telemetry. A caller reads the Completion
	// for the figures and the error for the outcome.
	Complete(ctx context.Context, prompt Prompt) (Completion, error)
}

// Prompt is one rendered ask, in the shape every provider is given it.
type Prompt struct {
	Model string
	// System is the preamble(system prompt).
	System []Block
	// Content is the rendered deliberation, in the order the ordinals in the answer refer to.
	Content []Block
	Schema  Schema
	// Tools are the native tool definitions, generated from the one declarative
	// contract — never a second hand-written representation. Present, they put the
	// prompt in tool-calling mode: the adapter translates each into its vendor's wire
	// shape, and the answer may carry tool calls.
	Tools []integrations.ToolDefinition
	// ForceTool names the one tool the answer must call — the forced concluding turn.
	ForceTool string
	// Turns is the conversation so far, oldest first: each the assistant's own prior
	// move with what answered it. The adapter replays them verbatim, because caching is
	// a prefix match and the transcript is the prefix.
	Turns []Turn
	// MaxOutputTokens bounds the answer. On providers where thinking and answer text share the
	// bound, a value sized around the answer alone truncates mid-thought.
	MaxOutputTokens int64
	// Effort is how hard to think. It is the primary resource and latency lever and the right value
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
type Schema struct {
	Name     string
	Version  string
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
type Effort string

const (
	EffortLow       Effort = "low"
	EffortMedium    Effort = "medium"
	EffortHigh      Effort = "high"
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

// What one call consumed.
//
// The distinction this file exists to keep is between a figure of zero and no figure at all. Zero
// is a measurement — the cache missed. Absent is the lack of one — the provider never said.
// Collapsing them makes a cache that stopped working indistinguishable from a provider that does
// not report caching, and the first is an incident while the second is a Tuesday.

// Count is a token figure and whether the provider actually reported it.
type Count struct {
	Tokens int64
	// Reported is false when the provider said nothing about this figure. A consumer that treats
	// an unreported count as zero is asserting a measurement nobody made.
	Reported bool
}

// Counted is a figure a provider reported.
func Counted(tokens int64) Count { return Count{Tokens: tokens, Reported: true} }

// Unreported is the absence of a figure.
func Unreported() Count { return Count{} }

// Or returns the figure, or the fallback when the provider reported none. It exists so the few
// places that genuinely must have a number say so at the call site.
func (c Count) Or(fallback int64) int64 {
	if !c.Reported {
		return fallback
	}
	return c.Tokens
}

// TokenUsage is one call's consumption, normalized across every provider.
type TokenUsage struct {
	Input      Count
	Output     Count
	CacheWrite Count
	CacheRead  Count
	// Reasoning is tokens spent on internal reasoning where the provider breaks them out. They
	// are already inside Output on every provider that reports both; this is a decomposition for
	// observability, never an addend.
	Reasoning Count
}

func usageOf(usage TokenUsage) investigation.Usage {
	return investigation.Usage{
		InputTokens:  usage.Input.Or(0) + usage.CacheRead.Or(0) + usage.CacheWrite.Or(0),
		OutputTokens: usage.Output.Or(0),
	}
}

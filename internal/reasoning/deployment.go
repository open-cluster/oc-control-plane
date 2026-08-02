package reasoning

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Which vendor and which model answer, as configuration rather than as constants.
//
// A model is a deployment choice that moves with price, availability, regional obligation and what
// a tenant will consent to. Every one of those becomes a release if the choice is compiled in, so
// it is resolved at startup, validated there, and pinned into the round that used it.

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
	// Effort is how hard to think, the primary cost and latency lever.
	Effort Effort
	// MaxOutputTokens bounds one answer. It is set generously where thinking and answer share the
	// bound, because a value sized around the answer alone truncates mid-thought.
	MaxOutputTokens int64
	// MaxPromptTokens refuses an oversized deliberation before it is sent, where the provider can
	// measure one. Zero means no pre-flight bound.
	MaxPromptTokens int64
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
	// MaxConcurrent bounds in-flight requests to this deployment, so one investigation cannot
	// consume the capacity of every other. Zero means the package default.
	MaxConcurrent int
}

// Bounds that are safe rather than ambitious. A deployment that names none of them gets these,
// and every one of them is a restriction rather than a target.
const (
	defaultMaxOutputTokens = 32_000
	defaultRequestTimeout  = 120 * time.Second
	defaultMaxAttempts     = 3
	defaultMaxConcurrent   = 4
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
	if d.MaxConcurrent <= 0 {
		d.MaxConcurrent = defaultMaxConcurrent
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
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf(
				"the %s deployment names a base url that is not an https host; the adapter may "+
					"reach that host and nothing else, so it has to be one", d.Provider)
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

// Consent is which providers may be sent this deployment's evidence.
//
// It is checked when a deployment is selected and AGAIN at every fallback hop, because consenting
// to one vendor is not consenting to the next — a fallback that inherited the head of the chain's
// consent would be an undisclosed subprocessor change performed automatically.
//
// This slice resolves consent per deployment rather than per organization. The model boundary
// carries no organization: this repository refuses ambient tenancy, so a tenant identifier is
// passed explicitly or not at all, and the boundary's three methods pass evidence rather than
// tenants. Per-tenant consent therefore needs a wider domain interface, which this slice may not
// change. A single-tenant deployment is fully covered; a shared one serving organizations with
// different subprocessor agreements is NOT, and the gap is stated here rather than left for
// someone to infer from an empty set.
type Consent struct {
	providers map[string]struct{}
}

// ConsentTo records the providers evidence may be sent to.
func ConsentTo(providers ...string) Consent {
	permitted := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if trimmed := strings.TrimSpace(provider); trimmed != "" {
			permitted[trimmed] = struct{}{}
		}
	}
	return Consent{providers: permitted}
}

// Permits reports whether a provider may see evidence. Nothing recorded permits nothing: absent a
// recorded consent the round fails rather than defaulting to allowed, because defaulting the other
// way makes the safe case the one an operator has to remember to configure.
func (c Consent) Permits(provider string) bool {
	_, permitted := c.providers[provider]
	return permitted
}

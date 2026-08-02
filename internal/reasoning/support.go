package reasoning

import "fmt"

// What each provider can do, declared rather than assumed.
//
// The type is called Support rather than Capabilities because Capability is already this system's
// word for something else entirely — a named, versioned, frozen contract for one bounded read a
// Relay performs. Two meanings for one word inside one program is how a glossary stops being one.
//
// The orchestration reads this declaration before it relies on anything. The point is not to
// catalogue features: it is that a capability one vendor lacks must degrade VISIBLY. A provider
// that cannot enforce an output schema still has its answer validated against the same schema and
// still fails the round on a persistent violation — the guarantee holds, the enforcement point
// moves, and the round records which one it got. Silently treating an absent capability as
// satisfied is how a guarantee becomes a belief.

// Support is one provider's declaration.
type Support struct {
	// StrictStructuredOutput reports whether the provider itself constrains the answer to the
	// declared schema. Where it is false the schema is enforced here instead, which costs a retry
	// rather than a guarantee.
	StrictStructuredOutput bool
	// TokenCounting reports whether the provider can price a prompt in tokens before it is sent.
	// Where it is false the pre-flight size check is recorded as skipped, never as passed.
	TokenCounting bool
	// Streaming reports whether the provider streams. It matters because a large output ceiling
	// on a non-streaming request risks a transport timeout well before the model is finished.
	Streaming bool
	// Caching reports whether the provider caches a stable prompt prefix and says so in its
	// usage figures. Where it is false the cache columns of a record are absent, not zero.
	Caching bool
	// RefusalDetection reports whether the provider distinguishes its own safeguards declining
	// from every other way a request can end. Where it is false, a refusal is indistinguishable
	// from a short answer and this system cannot promise to tell them apart.
	RefusalDetection bool
	// ProviderSideFallback reports whether the provider can re-serve a declined request on
	// another model inside the same call. It is a capability rather than a default, and when it
	// fires the answering model is read from the response rather than assumed.
	ProviderSideFallback bool
	// RegionalOrZeroRetention reports whether the provider can be operated under a data-residency
	// or zero-retention obligation. It gates which tenants a deployment may serve.
	RegionalOrZeroRetention bool
}

// Describe renders the matrix for a startup log, so an operator can see what the configured
// providers actually promise rather than reading the code to find out.
func (s Support) Describe() string {
	return fmt.Sprintf(
		"strict_output=%t token_counting=%t streaming=%t caching=%t refusal_detection=%t "+
			"provider_fallback=%t regional_or_zero_retention=%t",
		s.StrictStructuredOutput, s.TokenCounting, s.Streaming, s.Caching,
		s.RefusalDetection, s.ProviderSideFallback, s.RegionalOrZeroRetention)
}

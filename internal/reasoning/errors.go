package reasoning

import (
	"errors"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE FAILURE OUTCOMES, AND WHY THEY ARE TOLD APART.
//
// Each of these calls for something different from a different person, and collapsing any two of
// them loses that. A rejected request pages the engineer who shipped this build; an outage pages
// whoever watches the vendor. The one that matters most is the distinction between all of them and
// an ABSTENTION: an abstention says no explanation was supported, and every failure here says
// nobody looked. Reporting the first when the second is true is the single most damaging thing
// this component could do, so an abstention is not in this enumeration and never shares a code
// path with one.

var (
	// ErrRefused is the provider's own safeguards declining. It arrives as a successful response
	// carrying a refusal rather than as a transport error, which is why the stop reason is read
	// before the document is.
	ErrRefused = errors.New("the model provider declined the request")
	// ErrOutage is the provider being unreachable, rate-limited past the retries it allows, or failing
	// on its own side.
	ErrOutage = errors.New("the model provider is unreachable")
	// ErrRejected is the provider saying this request is malformed. It is a defect in this build
	// and must not be disguised as an outage, or the wrong person is paged.
	ErrRejected = errors.New("the model provider rejected the request as malformed")
	// ErrMalformed is an answer that did not satisfy the declared schema.
	ErrMalformed = errors.New("the model's answer did not satisfy the declared schema")
	// ErrTimeout is the deadline being reached.
	ErrTimeout = errors.New("the model provider did not answer within the deadline")
	// ErrCeilingReached is the spending ceiling reached before a request was sent.
	ErrCeilingReached = errors.New("the reasoning cost ceiling has been reached")
	// ErrUnpriced is a model with no declared rate. It is refused rather than costed at zero.
	ErrUnpriced = errors.New("no rate is declared for this model")
	// ErrNotConsented is a provider that may not be sent this deployment's evidence.
	ErrNotConsented = errors.New("this deployment has not consented to this model provider")
)

// Outcome is which named failure happened.
//
// The values are recorded in telemetry and audit entries, so they are this system's own
// vocabulary rather than any vendor's spelling of the same idea.
type Outcome int16

const (
	OutcomeRefused Outcome = iota + 1
	OutcomeOutage
	OutcomeRejected
	OutcomeMalformed
	OutcomeTimeout
	OutcomeCeilingReached
	OutcomeNotConsented
	OutcomeUnpriced
)

func (o Outcome) String() string {
	switch o {
	case OutcomeRefused:
		return "refused"
	case OutcomeOutage:
		return "outage"
	case OutcomeRejected:
		return "rejected_request"
	case OutcomeMalformed:
		return "malformed_output"
	case OutcomeTimeout:
		return "timeout"
	case OutcomeCeilingReached:
		return "cost_ceiling_reached"
	case OutcomeNotConsented:
		return "not_consented"
	case OutcomeUnpriced:
		return "unpriced_model"
	default:
		return "unrecognised"
	}
}

// sentinel is the error each outcome reads as, so a caller can ask errors.Is about the specific
// thing that went wrong rather than string-matching a message.
func (o Outcome) sentinel() error {
	switch o {
	case OutcomeRefused:
		return ErrRefused
	case OutcomeOutage:
		return ErrOutage
	case OutcomeRejected:
		return ErrRejected
	case OutcomeMalformed:
		return ErrMalformed
	case OutcomeTimeout:
		return ErrTimeout
	case OutcomeCeilingReached:
		return ErrCeilingReached
	case OutcomeNotConsented:
		return ErrNotConsented
	case OutcomeUnpriced:
		return ErrUnpriced
	default:
		return nil
	}
}

// Recoverable reports whether another deployment could plausibly answer what this one would not.
//
// It is what the configured fallback chain consults. A rejected request is this build's own defect
// and would be rejected identically by every hop, so trying the next one spends money to fail
// again; a malformed answer is retried inside the deployment that produced it rather than escaped
// from. Only a refusal, an outage and a timeout are things a different model might survive.
func (o Outcome) Recoverable() bool {
	switch o {
	case OutcomeRefused, OutcomeOutage, OutcomeTimeout:
		return true
	default:
		return false
	}
}

// Failure is one named failure with the deployment it happened on.
//
// It wraps the domain's model-unavailable error as well as its own sentinel. That is deliberate:
// the runner turns model-unavailable into a FAILED round with the reasoning step recorded as a
// coverage gap, which is the honest ending for every outcome here — and the specific sentinel
// stays available to telemetry, audit records and tests that need to tell them apart.
type Failure struct {
	Outcome  Outcome
	Provider string
	Model    string
	// Detail is what the provider said, already stripped of anything that could carry evidence
	// content or a credential.
	Detail string
	// Category is the provider's own classification of a refusal, where it gives one. It is the
	// difference between "declined for a security topic" and "declined, no reason offered".
	Category string
	cause    error
}

func (f *Failure) Error() string {
	message := fmt.Sprintf("%s: %s/%s", f.Outcome, f.Provider, f.Model)
	if f.Category != "" {
		message += " (" + f.Category + ")"
	}
	if f.Detail != "" {
		message += ": " + f.Detail
	}
	return message
}

// Unwrap returns both the outcome's own sentinel and the domain's model-unavailable error, so
// errors.Is answers yes to the specific failure and to the general one the round is ended by.
func (f *Failure) Unwrap() []error {
	unwrapped := []error{investigation.ErrModelUnavailable}
	if sentinel := f.Outcome.sentinel(); sentinel != nil {
		unwrapped = append(unwrapped, sentinel)
	}
	if f.cause != nil {
		unwrapped = append(unwrapped, f.cause)
	}
	return unwrapped
}

// Failed builds a named failure.
func Failed(outcome Outcome, provider, model, detail string) *Failure {
	return &Failure{Outcome: outcome, Provider: provider, Model: model, Detail: detail}
}

// FailedBecause builds a named failure that keeps an underlying cause reachable, for a transport
// error worth inspecting further up.
func FailedBecause(outcome Outcome, provider, model, detail string, cause error) *Failure {
	return &Failure{
		Outcome: outcome, Provider: provider, Model: model, Detail: detail, cause: cause,
	}
}

// OutcomeOf reports the named outcome behind an error, and whether there was one. An error from
// somewhere else in the program is not forced into this vocabulary.
func OutcomeOf(err error) (Outcome, bool) {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Outcome, true
	}
	return 0, false
}

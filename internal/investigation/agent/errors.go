package agent

import (
	"errors"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var ErrModelUnavailable = investigation.ErrReasonerUnavailable

var (
	ErrRefused        = errors.New("the model provider declined the request")
	ErrOutage         = errors.New("the model provider is unreachable")
	ErrRejected       = errors.New("the model provider rejected the request as malformed")
	ErrMalformed      = errors.New("the model's answer did not satisfy the declared schema")
	ErrTimeout        = errors.New("the model provider did not answer within the deadline")
	ErrCeilingReached = errors.New("the reasoning cost ceiling has been reached")
	ErrUnpriced       = errors.New("no rate is declared for this model")
	ErrNotConsented   = errors.New("this deployment has not consented to this model provider")
)

// Outcome is which named failure happened.
type Outcome int16

const (
	OutcomeRefused Outcome = iota + 1
	OutcomeOutage
	OutcomeRejected
	OutcomeMalformed
	OutcomeTimeout
	OutcomeCeilingReached
	OutcomeNotConsented
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
	default:
		return nil
	}
}

// Failure is one named failure with the deployment it happened on.
type Failure struct {
	Outcome  Outcome
	Provider string
	Model    string
	Detail   string
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
	unwrapped := []error{ErrModelUnavailable}
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

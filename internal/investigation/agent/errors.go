package agent

import (
	"errors"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

var ErrModelUnavailable = investigation.ErrReasonerUnavailable

var (
	ErrRefused                  = errors.New("model provider declined the request")
	ErrOutage                   = errors.New("model provider is unreachable")
	ErrRejected                 = errors.New("model provider rejected the request")
	ErrMalformed                = errors.New("model response does not match the schema")
	ErrTimeout                  = errors.New("model provider timed out")
	ErrContextWindow            = errors.New("model context window exceeded")
	errUnavailablePriorEvidence = errors.New("a finding cites unavailable prior evidence; cite current reads with run_refs, and use evidence_refs only for supplied prior Investigation/ordinal pairs; otherwise leave evidence_refs empty")
)

type Outcome int16

const (
	OutcomeRefused Outcome = iota + 1
	OutcomeOutage
	OutcomeRejected
	OutcomeMalformed
	OutcomeTimeout
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
	default:
		return "unrecognised"
	}
}

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
	default:
		return nil
	}
}

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

func Failed(outcome Outcome, provider, model, detail string) *Failure {
	return &Failure{
		Outcome:  outcome,
		Provider: provider,
		Model:    model,
		Detail:   detail,
	}
}

func FailedBecause(outcome Outcome, provider, model, detail string, cause error) *Failure {
	return &Failure{
		Outcome:  outcome,
		Provider: provider,
		Model:    model,
		Detail:   detail,
		cause:    cause,
	}
}

func ContextRejected(provider, model, detail string, cause error) *Failure {
	return FailedBecause(OutcomeRejected, provider, model, detail,
		errors.Join(ErrContextWindow, cause))
}

func OutcomeOf(err error) (Outcome, bool) {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Outcome, true
	}
	return 0, false
}

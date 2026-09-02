package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
)

// Which failure is which, because they call for different things from different people.
//
// The distinction this file exists to keep is between an OUTAGE and a REJECTED REQUEST. An outage
// is the vendor's problem and pages whoever watches them; a rejected request is a defect in this
// build and pages the engineer who shipped it. Disguising the second as the first is how a bug
// spends a week being investigated as someone else's incident.

// classify turns an HTTP status into a named outcome.
func classify(provider, model string, status int, identifier string, cause error) error {
	detail := fmt.Sprintf("the provider answered %d", status)
	if identifier != "" {
		detail += " (request " + identifier + ")"
	}

	switch {
	case status == 429:
		// Retries are already exhausted by the time this surfaces: the client backs off and
		// retries rate limits itself, so reaching here means the budget for them ran out.
		return reasoning.FailedBecause(reasoning.OutcomeOutage, provider, model,
			detail+": rate limited past the retries this deployment allows", cause)
	case status >= 500:
		return reasoning.FailedBecause(reasoning.OutcomeOutage, provider, model,
			detail+": the provider failed on its own side", cause)
	case status == 401 || status == 403:
		// A credential problem is this deployment's, not the vendor's. It is a rejected request
		// rather than an outage so that the person who can fix it is the person who is told.
		return reasoning.FailedBecause(reasoning.OutcomeRejected, provider, model,
			detail+": the credential was not accepted", cause)
	case status == 404:
		return reasoning.FailedBecause(reasoning.OutcomeRejected, provider, model,
			detail+": the model identifier is not one this provider serves", cause)
	case status == 413:
		return reasoning.FailedBecause(reasoning.OutcomeRejected, provider, model,
			detail+": the request was larger than the provider accepts", cause)
	case status >= 400:
		return reasoning.FailedBecause(reasoning.OutcomeRejected, provider, model,
			detail+": the provider refused the request as malformed", cause)
	default:
		return reasoning.FailedBecause(reasoning.OutcomeOutage, provider, model, detail, cause)
	}
}

// transportFailure names what went wrong before a status existed.
//
// A cancelled context is a timeout rather than an outage: the round's own deadline stopped the
// call, and reporting the vendor as unreachable for it would send someone to look at a service that
// was answering.
func transportFailure(provider, model string, cause error) error {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, provider, model,
			"the round's deadline was reached before the provider answered", cause)
	case errors.Is(cause, context.Canceled):
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, provider, model,
			"the investigation was cancelled while waiting for the provider", cause)
	}

	var timeout net.Error
	if errors.As(cause, &timeout) && timeout.Timeout() {
		return reasoning.FailedBecause(reasoning.OutcomeTimeout, provider, model,
			"the provider did not answer within this deployment's request timeout", cause)
	}
	return reasoning.FailedBecause(reasoning.OutcomeOutage, provider, model,
		"the provider could not be reached", cause)
}

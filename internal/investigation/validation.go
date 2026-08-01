package investigation

import (
	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/capability"
)

// Admission is a proposal after validation: either a read that may be dispatched, or a refusal
// with its reason. Both are recorded — a request refused before dispatch is part of the record of
// what was tried — and only the first ever reaches a customer's cluster.
type Admission struct {
	Request  Request
	Refusal  Refusal
	Admitted bool
}

// Bounds is what a proposal is checked against. It travels as one value because the checks are not
// independent: a read is in scope only in relation to the case's scope, within its execution
// limits only in relation to what the round has already spent, and justified only in relation to
// the hypotheses the planner was shown.
type Bounds struct {
	Scope    Scope
	Window   Window
	Controls Controls
	// Spent is what the round has consumed so far.
	Spent Spend
	// Hypotheses are what the planner was shown, in the order its ordinals refer to.
	Hypotheses []Hypothesis
	// KnownPods are the pods the brief resolved for this workload. A log read may name one of
	// these and nothing else: without it, "read the logs of a pod in this namespace" would be a
	// read of any workload the namespace happens to hold, which is a wider scope than the case has.
	KnownPods []string
	// Pass is which adaptive pass proposed this. Zero is the opening plan.
	Pass int
}

// Admit checks one proposal against everything it has to satisfy BEFORE anything is dispatched.
//
// The order is deliberate: the cheapest and most structural refusals come first, so a read that
// could never have been made is refused without encoding it, and the encoded form is validated
// last because the bytes are what actually travel. The Relay re-validates on receipt as it already
// does — this refuses the same thing before it costs a dispatch, a lease and a round trip, and it
// means a Relay is never the only thing standing between a planner's mistake and a customer's
// cluster.
func Admit(proposal Proposal, bounds Bounds) Admission {
	// Encoded first, and kept on the request whatever happens to it. A refused read is part of the
	// record of what was tried, and a record that says a read was refused without saying what it
	// asked for is one nobody can learn from. Encoding is pure and dispatches nothing.
	encoded, encodable := proposal.Arguments.Encode(proposal.CapabilityID)

	request := Request{
		Pass:              bounds.Pass,
		CapabilityID:      proposal.CapabilityID,
		CapabilityVersion: proposal.CapabilityVersion,
		Arguments:         encoded,
		Reason:            proposal.Reason,
	}

	if !capability.Known(proposal.CapabilityID, proposal.CapabilityVersion) {
		return refused(request, RefusedUnknownCapability)
	}
	if !bounds.Controls.Permits(proposal.CapabilityID) {
		return refused(request, RefusedNotPermitted)
	}

	// The containment mechanism, checked before anything else about the read. An adaptive read
	// must point at a typed hypothesis a human can read; the chain from evidence text to a further
	// read passes through it and may not go around it.
	justification, ok := justify(proposal, bounds)
	if !ok {
		return refused(request, RefusedUnjustified)
	}
	request.Justification = justification

	if refusal := checkScope(proposal, bounds); refusal != 0 {
		return refused(request, refusal)
	}
	if refusal := checkLimits(bounds); refusal != 0 {
		return refused(request, refusal)
	}

	if !encodable {
		return refused(request, RefusedArguments)
	}
	// The bytes are validated rather than the fields, because the bytes are what would travel.
	// Validating a message and then encoding it would leave the encoding unchecked.
	if err := capability.Validate(
		proposal.CapabilityID, proposal.CapabilityVersion, encoded); err != nil {
		return refused(request, RefusedArguments)
	}

	request.State = RequestProposed
	return Admission{Request: request, Admitted: true}
}

// justify resolves the hypothesis a read points at. The opening plan precedes every hypothesis and
// is the one exemption; everything after it must name one that exists among what the planner was
// shown, by ordinal rather than by identifier.
func justify(proposal Proposal, bounds Bounds) (uuid.UUID, bool) {
	if bounds.Pass == 0 {
		return uuid.Nil, proposal.Justification == 0
	}
	if proposal.Justification < 1 || proposal.Justification > len(bounds.Hypotheses) {
		return uuid.Nil, false
	}
	return bounds.Hypotheses[proposal.Justification-1].ID, true
}

// checkScope refuses a read that would leave the investigation's scope or its window.
//
// This is the check that stops a read for one thing becoming a read for another. It is per
// capability because what "in scope" means differs: a workload read must name this workload, an
// events read must stay inside this namespace and this window, and a log read must name a pod the
// brief actually resolved for this workload.
func checkScope(proposal Proposal, bounds Bounds) Refusal {
	arguments := proposal.Arguments
	if arguments.Namespace != bounds.Scope.Namespace {
		return RefusedOutOfScope
	}

	switch proposal.CapabilityID {
	case capability.KubernetesWorkloadRuntime:
		if arguments.WorkloadName != bounds.Scope.WorkloadName ||
			arguments.WorkloadKind != bounds.Scope.WorkloadKind {
			return RefusedOutOfScope
		}
	case capability.KubernetesNamespaceEvents:
		if arguments.Window.Start.IsZero() || arguments.Window.End.IsZero() {
			return RefusedOutOfWindow
		}
		// Inside the case's window at both ends. A read reaching outside it would gather evidence
		// about a moment this case is not looking at and put it on the same timeline.
		if arguments.Window.Start.Before(bounds.Window.Start) ||
			arguments.Window.End.After(bounds.Window.End) {
			return RefusedOutOfWindow
		}
	case capability.KubernetesContainerLogs:
		if arguments.ContainerName == "" {
			return RefusedArguments
		}
		if !known(bounds.KnownPods, arguments.PodName) {
			// Not merely unknown: a pod the brief did not resolve for this workload is another
			// workload's, and reading it would widen the scope the case was opened under.
			return RefusedOutOfScope
		}
	}
	return 0
}

// checkLimits refuses a read the round has no allowance left for. Reaching a limit is not a
// failure — the round terminates and concludes or abstains on what it has — but the read that would
// have crossed the line is recorded and not sent.
func checkLimits(bounds Bounds) Refusal {
	if bounds.Controls.MaxRequests > 0 && bounds.Spent.Requests >= bounds.Controls.MaxRequests {
		return RefusedLimitReached
	}
	if bounds.Controls.MaxResultBytes > 0 &&
		bounds.Spent.ResultBytes >= bounds.Controls.MaxResultBytes {
		return RefusedLimitReached
	}
	if bounds.Controls.MaxMicroCents > 0 &&
		bounds.Spent.MicroCents >= bounds.Controls.MaxMicroCents {
		return RefusedLimitReached
	}
	return 0
}

func refused(request Request, refusal Refusal) Admission {
	request.State = RequestRefused
	request.Refusal = refusal
	return Admission{Request: request, Refusal: refusal}
}

func known(pods []string, named string) bool {
	for _, pod := range pods {
		if pod == named {
			return true
		}
	}
	return false
}

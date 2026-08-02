package investigation

import (
	"context"
	"errors"
)

// SEAM 3 — THE MODEL BOUNDARY, AND THE ONE COMPONENT IN THIS PROGRAM THAT IS FAKED IN CI.
//
// test/e2e/doc.go states the rule this deviates from: nothing there is a test double, because
// every component that could be faked is one whose behaviour is in question. The model is the
// first genuine exception. It is nondeterministic and priced per call, and what is in question
// about it — whether its explanations are any good — is answered by the scenario harness against a
// live provider, not by a commit gate. A CI job that failed on ordinary model variance is a gate
// that gets ignored within two weeks.
//
// The deviation is recorded here, at the seam, so the next reader does not conclude the rule was
// forgotten.
//
// Two properties of this interface are structural rather than stylistic. A Reasoner never names a
// row — it cites by ordinal among what it was shown — and it never emits bytes, only typed
// Arguments. Both exist so that a model response, which may have been steered by text from a
// customer's systems, cannot reach storage or a customer's cluster as anything but a proposal this
// control plane then validates.

// ErrModelUnavailable reports the provider being unreachable or refusing. It produces a FAILED
// round rather than a conclusion: an outage must produce an honest failure and never a guess, and
// "the model was down" is a platform failure rather than "no explanation was found".
var ErrModelUnavailable = errors.New("the model provider is unavailable")

// ErrTranscriptKeyMismatch reports a recording made for different components than the ones asking
// for it. It fails loudly rather than replaying: a stale recording that replays silently is a CI
// run that proves the wrong build works.
var ErrTranscriptKeyMismatch = errors.New("the recorded transcript is for different components")

// NamedFailure is a model-boundary failure that can say WHICH failure it was.
//
// Every one of them ends the round the same way, and for a while the case file said so in the same
// sentence — "the reasoning step could not run" — for an outage, a provider refusal, a persistently
// malformed answer and a cost ceiling alike. They are distinguishable in logs, but the case file is
// what an on-call engineer and a blind scorer read, and there they call for completely different
// things: one is the vendor's problem, one is this build's defect, one is an operator's limit doing
// its job.
//
// The name is a closed vocabulary word and nothing else. It deliberately carries no provider
// message, because a case file is meant to be safe to share and a vendor's error text is not this
// system's to vouch for.
type NamedFailure interface {
	error
	FailureName() string
}

// failureName reports what kind of model-boundary failure this was, or "unnamed" for one that
// cannot say. It is the domain's own vocabulary check: infrastructure satisfies the interface, and
// the domain never learns that a provider exists.
func failureName(cause error) string {
	var named NamedFailure
	if errors.As(cause, &named) {
		return named.FailureName()
	}
	return "unnamed"
}

// Reasoner is the model boundary. It does two things and they are recorded separately: it proposes
// hypotheses, and it proposes reads. Keeping them apart is what makes evidence selection scorable
// independently of the conclusion (ADR-009), which is the only way to tell a bad conclusion from
// bad evidence selection.
type Reasoner interface {
	// Hypotheses proposes competing explanations from the brief alone. No evidence text has been
	// gathered when this is called, which is what makes the opening comparable between runs.
	Hypotheses(ctx context.Context, brief Brief) (Hypothesized, error)
	// Requests proposes further typed reads from the hypotheses currently held. Each one points at
	// the hypothesis it would support or falsify: the chain from "a log line said X" to "therefore
	// read Y" always passes through a typed hypothesis a human can read (ADR-012).
	Requests(ctx context.Context, over Deliberation) (Proposed, error)
	// Conclude states the most supported explanation or abstains. What comes back is a draft: it
	// is checked against the output schema before anything is persisted, so a response containing
	// an uncited claim never reaches storage.
	Conclude(ctx context.Context, over Deliberation) (Concluded, error)
}

// Usage is what one call to the boundary consumed. It travels with every answer rather than being
// asked for afterwards, because a caller that has to remember to ask is a caller that will
// eventually not, and an unpriced feature cannot be enabled.
type Usage struct {
	Tokens     int64
	MicroCents int64
}

// Hypothesized is what the planner proposed, with what proposing it cost.
type Hypothesized struct {
	Hypotheses []Hypothesis
	Usage      Usage
}

// Proposed is the reads the planner wants next, what it made of the evidence it has been shown so
// far, and what choosing them cost.
type Proposed struct {
	Proposals []Proposal
	// Hypotheses are explanations this pass proposed that the opening could not have: the brief
	// alone does not contain them. They are appended to the round and take the next ordinals.
	Hypotheses []Hypothesis
	Weighings  []Weighing
	Settlings  []Settling
	Usage      Usage
}

// Concluded is the draft outcome, the last movements of the hypotheses, and what reaching it cost.
type Concluded struct {
	Draft Draft
	// Hypotheses are explanations the evidence produced that nothing earlier proposed.
	//
	// The opening call sees the brief alone, so a cause only the evidence could reveal — a log line
	// naming a Secret nobody knew was referenced — cannot be among the explanations it proposed.
	// With no way to add one, a reasoner that found such a cause had two moves: state it attached
	// to no hypothesis, or abstain on evidence naming the answer. Both are worse than letting it
	// say what it found and what would disprove it. These may therefore be settled by this same
	// answer and may be the one the outcome explains.
	Hypotheses []Hypothesis
	Weighings  []Weighing
	Settlings  []Settling
	Usage      Usage
}

// Weighing is how one EvidenceItem stands towards one Hypothesis, both named by their ordinal
// among what the reasoner was shown. Neutral weighings are worth recording: what was considered
// and moved nothing is what shows a hypothesis was examined rather than ignored.
type Weighing struct {
	Hypothesis int
	Evidence   int
	Stance     Stance
	Reason     string
}

// Settling moves one hypothesis, by its ordinal among what the reasoner was shown. Setting one
// aside carries the reason, because an alternative set aside silently is one a reader cannot check.
type Settling struct {
	Hypothesis int
	State      HypothesisState
	Reason     string
}

// Deliberation is everything the reasoner is shown, in the order the ordinals it answers with
// refer to.
//
// Evidence content reaches the model here, bounded and after Relay-side redaction, marked as data
// at the boundary and never presented as instruction. Containment is structural rather than a
// matter of prompt wording: whatever this text says, the only thing it can produce is a Proposal
// or a Draft, and both are validated by the caller before they can affect a customer's cluster or
// this system's storage.
type Deliberation struct {
	Brief      Brief
	Hypotheses []Hypothesis
	Evidence   []Item
	Gaps       []Gap
	// Available is what may be asked for. A planner is given the list rather than a vocabulary, so
	// it cannot propose a read that does not exist in this Environment.
	Available []CapabilityRef
	// Remaining is how many more reads the round may make. It is shown so a planner can spend what
	// is left deliberately rather than discovering the bound by being refused.
	Remaining int
	// Pass is which adaptive pass this is, from one.
	Pass int
}

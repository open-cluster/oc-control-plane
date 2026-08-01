package investigation

import (
	"time"

	"github.com/google/uuid"
)

// RequestState is where one proposed capability read has got to. Requests that returned nothing
// useful are kept in the record with everything else, because the record has to show what was
// tried and not only what worked — that is the dataset which tells which reads earn their place.
//
// The values are persisted and frozen by a gate in internal/gates.
type RequestState int16

const (
	// RequestProposed is chosen by the planner and not yet validated.
	RequestProposed RequestState = iota + 1
	// RequestRefused failed validation and was NEVER dispatched. Nothing reached a customer's
	// cluster, and the reason is recorded.
	RequestRefused
	// RequestDispatched is a relay job enqueued and awaiting its result.
	RequestDispatched
	// RequestAnswered produced at least one EvidenceItem.
	RequestAnswered
	// RequestUnproductive returned and produced no EvidenceItem. It is not a failure and is not
	// discarded: what was asked, why, and that it yielded nothing is the record.
	RequestUnproductive
	// RequestFailed did not return — a timeout, a relay failure, a cancelled execution.
	RequestFailed
)

func (s RequestState) String() string {
	switch s {
	case RequestProposed:
		return "proposed"
	case RequestRefused:
		return "refused"
	case RequestDispatched:
		return "dispatched"
	case RequestAnswered:
		return "answered"
	case RequestUnproductive:
		return "unproductive"
	case RequestFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// Refusal is why a proposed read was not dispatched. Every one of these is decided in the control
// plane BEFORE anything is sent, and the Relay re-validates on receipt as it already does — so a
// planner's mistake is refused twice and reaches a customer's cluster never.
//
// The values are persisted and frozen by a gate in internal/gates.
type Refusal int16

const (
	// RefusedUnknownCapability names a capability, or a version of one, this build cannot
	// dispatch.
	RefusedUnknownCapability Refusal = iota + 1
	// RefusedNotPermitted names one local policy excludes.
	RefusedNotPermitted
	// RefusedOutOfScope names a namespace, workload or Connection outside the investigation's
	// scope. This is the refusal that stops a read for one thing becoming a read for another.
	RefusedOutOfScope
	// RefusedOutOfWindow names a time range outside the investigation's window.
	RefusedOutOfWindow
	// RefusedArguments could not produce a usable read.
	RefusedArguments
	// RefusedLimitReached means an execution limit had already been reached. The request is
	// recorded and not sent.
	RefusedLimitReached
	// RefusedUnjustified means no hypothesis justified it. A request must point at a typed
	// hypothesis a human can read; that is the chain evidence text may never bypass (ADR-012).
	RefusedUnjustified
	// RefusedConnection means the Connection could not carry it — disabled, trigger-only, or
	// served by a different Relay. The database decides this one on the insert; it is named here
	// so a refusal is recorded with the same vocabulary as the rest.
	RefusedConnection
)

func (r Refusal) String() string {
	switch r {
	case RefusedUnknownCapability:
		return "capability is not one this build dispatches"
	case RefusedNotPermitted:
		return "capability not permitted by local policy"
	case RefusedOutOfScope:
		return "outside the investigation's scope"
	case RefusedOutOfWindow:
		return "outside the investigation's window"
	case RefusedArguments:
		return "arguments could not produce a usable read"
	case RefusedLimitReached:
		return "an execution limit was already reached"
	case RefusedUnjustified:
		return "no hypothesis justifies this read"
	case RefusedConnection:
		return "connection cannot carry this read"
	default:
		return "unrecognised"
	}
}

// Request is one typed read the planner proposed, together with the hypothesis that justified it.
//
// The justification is persisted rather than derived, and it is the structural containment
// mechanism as much as it is an audit field: the planner may never derive a read from evidence
// text, so the chain from "a log line said X" to "therefore read Y" passes through a typed
// hypothesis a human can read. It is also what makes evidence selection scorable independently of
// the conclusion (ADR-009).
type Request struct {
	ID      uuid.UUID
	RoundID uuid.UUID
	Ordinal int
	// Pass is which adaptive pass proposed it. Zero is the opening plan, before any hypothesis
	// existed; one and two are the adaptive passes.
	Pass              int
	CapabilityID      string
	CapabilityVersion uint32
	// Arguments is the encoded capability argument message, exactly as it would be dispatched.
	// Stored so a round replays from its own record with no access to live sources.
	Arguments []byte
	// Justification is the hypothesis this read would support or falsify. Required on every
	// adaptive request; empty only for the opening plan's reads, which precede every hypothesis.
	Justification uuid.UUID
	// Reason is why this read bears on that hypothesis, in the planner's words.
	Reason string
	State  RequestState
	// Refusal is why it was not dispatched, and is set only when the state is refused.
	Refusal Refusal
	// JobID is the relay job this became. Zero when nothing was dispatched.
	JobID       uuid.UUID
	ProposedAt  time.Time
	SettledAt   time.Time
	ResultBytes int64
}

// Justified reports whether an adaptive request points at a hypothesis. The opening plan's reads
// precede every hypothesis and are the one exemption, which is why this takes the pass rather
// than reading the field alone.
func (r Request) Justified() bool {
	return r.Pass == 0 || r.Justification != uuid.Nil
}

// Proposal is what the planner emitted: a read it wants and the hypothesis it points at.
//
// It is deliberately NOT a Request. A proposal has not been validated, and giving the two one type
// is how an unvalidated read reaches a dispatcher. Two things about its shape are load-bearing:
//
// The justification is an ORDINAL among the hypotheses the planner was shown, not an identifier. A
// planner that could name a row could name one it invented, and an invented identifier looks like
// a citation until somebody follows it.
//
// The arguments are TYPED rather than encoded. A planner never emits bytes, so the bounds and the
// scope are checked against fields rather than against a payload that has to be decoded first —
// and there is no shape here for a command, a selector or a query.
type Proposal struct {
	CapabilityID      string
	CapabilityVersion uint32
	Arguments         Arguments
	// Justification is the one-based position of the hypothesis this read would support or
	// falsify, among those the planner was shown. Zero is legal only in the opening plan, which
	// precedes every hypothesis.
	Justification int
	Reason        string
}

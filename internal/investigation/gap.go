package investigation

import (
	"time"

	"github.com/google/uuid"
)

// GapCause is why something could not be checked. Each is a different thing for a reader to do
// about it, which is why they are told apart rather than collapsed into "unknown".
//
// The values are persisted and frozen by a gate in internal/gates.
type GapCause int16

const (
	// GapCapabilityUnavailable means no capability in this Environment can answer the question.
	GapCapabilityUnavailable GapCause = iota + 1
	// GapCapabilityNotPermitted means a capability exists and local policy refuses it. It reads
	// "not permitted by local policy", never "not available": those are different sentences and
	// only one tells an operator what to do (ADR-015).
	GapCapabilityNotPermitted
	// GapSourceUnreachable means the customer's system did not answer.
	GapSourceUnreachable
	// GapAuthorizationDenied means the read was refused by the source's own authorization.
	GapAuthorizationDenied
	// GapLimitReached means a bound this round ran under stopped the looking.
	GapLimitReached
	// GapRedactionMasked means the customer's OWN redaction policy withheld a field. It is a gap
	// with its cause rather than an empty field, so masking is visible rather than silent
	// (ADR-012), and so a customer's privacy policy does not degrade their investigations while
	// the product is blamed for the hole.
	GapRedactionMasked
	// GapRetentionHorizon means the source cannot answer for the whole window that was asked
	// for — Kubernetes Events expire on the cluster's own TTL and ReplicaSet history is bounded
	// by revisionHistoryLimit. The honest version of reading changes live rather than from a
	// change ledger, and the argument the ledger slice will be built on.
	GapRetentionHorizon
	// GapResultTruncated means the read stopped at a bound rather than finishing. It is why the
	// result cannot support an absence claim.
	GapResultTruncated
	// GapRequestRefused means a proposed read was refused before dispatch and never reached a
	// customer's cluster.
	GapRequestRefused
	// GapTargetNotFound means the source says the thing named is not there. It is a gap and never
	// absence evidence: a not-found from one read says nothing about whether it ever existed.
	GapTargetNotFound
	// GapExplanationUntested means the explanation this round landed on rests on a hypothesis no
	// dispatched read pointed at, so nothing was read that could have disproved it.
	//
	// None of the causes above fits: no capability was unavailable, no limit ran out, no source
	// declined. What ran out was OPPORTUNITY — the hypothesis arrived at the last call the round
	// makes, and there is no pass left in which to test it. That is why the consequence names
	// reinvestigation: a further round can do what this one could not.
	GapExplanationUntested
	// GapLedgerHorizon means the window opens before the change ledger's continuous knowledge of
	// this scope begins, so an absence of recorded change before that boundary is an absence of
	// recording, never an absence of change. Distinct from GapRetentionHorizon, which is about a
	// SOURCE's own memory: this one is about when the platform was watching.
	GapLedgerHorizon
	// GapLedgerStale means the ledger has not been confirmed current through the end of the
	// window — the Relay's synchronization is behind, faulted, or seeing a bounded page — so a
	// change late in the window may simply not have been recorded yet.
	GapLedgerStale
)

func (c GapCause) String() string {
	switch c {
	case GapCapabilityUnavailable:
		return "capability not available in this environment"
	case GapCapabilityNotPermitted:
		return "capability not permitted by local policy"
	case GapSourceUnreachable:
		return "source unreachable"
	case GapAuthorizationDenied:
		return "authorization denied by the source"
	case GapLimitReached:
		return "execution limit reached"
	case GapRedactionMasked:
		return "field masked by this organization's redaction policy"
	case GapRetentionHorizon:
		return "source cannot answer for the whole window"
	case GapResultTruncated:
		return "read truncated at a bound"
	case GapRequestRefused:
		return "request refused before dispatch"
	case GapTargetNotFound:
		return "target not found"
	case GapExplanationUntested:
		return "the explanation was never put at risk"
	case GapLedgerHorizon:
		return "the change ledger was not watching for the whole window"
	case GapLedgerStale:
		return "the change ledger is not confirmed current"
	default:
		return "unrecognised"
	}
}

// UntestedExplanationGap is what a round records beside an explanation it could not test.
//
// The wording lives here rather than at the call site because it is part of the standard: the
// caveat and the reason for it are written together, so a round cannot record one without the
// other.
func UntestedExplanationGap() Gap {
	return Gap{
		Cause:   GapExplanationUntested,
		Subject: "the hypothesis this round's explanation rests on",
		Consequence: "no read was dispatched to disprove the explanation this round settled on, " +
			"so it survived nothing; a further round can test it",
	}
}

// LedgerHorizonGap is what a round records when its window opens before the ledger's
// continuous knowledge of the scope begins. The wording lives here for the same reason
// UntestedExplanationGap's does: the boundary and its consequence are part of the standard.
func LedgerHorizonGap(coveredSince time.Time, window Window) Gap {
	return Gap{
		Cause: GapLedgerHorizon,
		Subject: "recorded changes between " + window.Start.UTC().Format(time.RFC3339) +
			" and " + coveredSince.UTC().Format(time.RFC3339),
		Consequence: "the change ledger has watched this scope only since " +
			coveredSince.UTC().Format(time.RFC3339) +
			"; an absence of recorded change before that is not an absence of change",
	}
}

// LedgerUnobservedGap is what a round records when watching has been requested for the
// scope and no baseline exists yet — the ledger can vouch for none of the window, and an
// empty change list must not read as a calm one.
func LedgerUnobservedGap() Gap {
	return Gap{
		Cause:   GapLedgerHorizon,
		Subject: "recorded changes for this scope",
		Consequence: "the change ledger has been asked to watch this scope and holds no " +
			"baseline yet, so an absence of recorded change says nothing about the window",
	}
}

// LedgerStaleGap is what a round records when the ledger cannot vouch for the window's tail:
// the last confirmed synchronization is behind the window's end, faulted, or truncated.
func LedgerStaleGap(confirmed time.Time, faulted, truncated bool) Gap {
	subject := "recorded changes after " + confirmed.UTC().Format(time.RFC3339)
	consequence := "the change ledger was last confirmed current at " +
		confirmed.UTC().Format(time.RFC3339) +
		", before the window closes; a change after that may not have been recorded yet"
	switch {
	case faulted:
		consequence = "the Relay's most recent synchronization could not read the cluster, " +
			"so the ledger's account of this window may stop at " +
			confirmed.UTC().Format(time.RFC3339)
	case truncated:
		consequence = "the most recent synchronization saw a bounded page rather than the whole " +
			"scope, so a change in the unseen remainder is invisible"
	}
	return Gap{Cause: GapLedgerStale, Subject: subject, Consequence: consequence}
}

// Gap is something the investigation could not check, and the consequence of not checking it. A
// gap with no consequence is a defect: it tells a reader that something is missing without
// telling them what it cost.
type Gap struct {
	ID      uuid.UUID
	RoundID uuid.UUID
	Ordinal int
	Cause   GapCause
	// CapabilityID is what could not be used, when the gap is about a capability. Empty when the
	// gap is about something else, such as a masked field.
	CapabilityID string
	// Subject names what could not be checked, in this investigation's own terms — a namespace, a
	// container, a field. Never the content that was withheld.
	Subject string
	// Consequence is what could not be concluded because of it. This is the field that makes a
	// gap useful rather than an apology.
	Consequence string
	RecordedAt  time.Time
}

// CoverageState is what is known about one typed capability in this case. There are five, and
// they are kept apart because collapsing any two of them loses the distinction a reader needs.
//
// These are NOT the glossary's Coverage states. CONTEXT.md defines Coverage as capability readiness
// in an Environment — available, degraded, stale, unauthorized, absent — which is a standing fact
// about what the platform could investigate there. This is a fact about what one CASE actually
// checked, and the two answer different questions: a capability can be available in an Environment
// and still have gone unread in a particular round. The divergence is deliberate and is worth
// stating, because the words are close enough that the next reader would otherwise assume one of
// them was written without looking at the other.
//
// The values are persisted and frozen by a gate in internal/gates.
type CoverageState int16

const (
	// CoverageChecked means the capability was used and produced evidence.
	CoverageChecked CoverageState = iota + 1
	// CoverageCheckedEmpty means it was used, found nothing, and carries a completeness
	// certificate — so the nothing is a fact rather than an absence of looking. This is the
	// certified negative, and it is the state that makes a complete read that found nothing
	// distinguishable from a read that never happened.
	CoverageCheckedEmpty
	// CoverageIncomplete means it was used and the read did not finish. No absence claim rests on
	// it.
	CoverageIncomplete
	// CoverageUnavailable means it is relevant here and could not be used.
	CoverageUnavailable
	// CoverageNotApplicable means the customer's stack does not provide it. A capability that
	// does not apply is NOT a gap and must never be reported as one: telling a team that their
	// Nomad cluster is missing Kubernetes events is how a coverage report stops being read.
	CoverageNotApplicable
)

func (s CoverageState) String() string {
	switch s {
	case CoverageChecked:
		return "checked"
	case CoverageCheckedEmpty:
		return "checked_empty"
	case CoverageIncomplete:
		return "incomplete"
	case CoverageUnavailable:
		return "unavailable"
	case CoverageNotApplicable:
		return "not_applicable"
	default:
		return "unrecognised"
	}
}

// IsGap reports whether this state is something missing. Not-applicable is not, which is the
// distinction the whole enumeration exists for.
func (s CoverageState) IsGap() bool {
	return s == CoverageIncomplete || s == CoverageUnavailable
}

// Coverage is one typed capability's state in this case, with the reason it is in that state.
// Coverage is reported per capability rather than as a percentage, because a percentage over
// unlike capabilities is a number nobody can act on and every reader assumes is a score.
type Coverage struct {
	CapabilityID      string
	CapabilityVersion uint32
	State             CoverageState
	// Reason is why it is in that state, in a sentence. Required: a state without one is a claim
	// with no basis.
	Reason string
	// Evidence counts the items this capability produced, so a reader can see that "checked"
	// means something arrived.
	Evidence int
}

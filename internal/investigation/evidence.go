package investigation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxEvidenceContentBytes bounds one evidence item's content on the write path AND on the read
// path. Bounding only the write path would leave a large item able to exhaust a client or a
// proxy on the way back out, which is the same defect from the other direction.
const MaxEvidenceContentBytes = 64 << 10

// MaxEvidenceStatementBytes bounds the one-line factual claim. It is short on purpose: a
// statement long enough to hold a payload is a payload.
const MaxEvidenceStatementBytes = 1024

// TrustClass is how a result's completeness was established. Relay-attested results carry
// confidence caps and are NEVER promoted to centrally verified — a promotion would make an
// attestation indistinguishable from a verification, which is the distinction the class exists
// to keep.
//
// The values are persisted and frozen by a gate in internal/gates.
type TrustClass int16

const (
	// TrustCentrallyVerified is completeness this control plane established itself.
	TrustCentrallyVerified TrustClass = iota + 1
	// TrustRelayAttested is completeness a Relay claimed. Recorded as such, capped, never
	// promoted.
	TrustRelayAttested
)

func (t TrustClass) String() string {
	switch t {
	case TrustCentrallyVerified:
		return "centrally_verified"
	case TrustRelayAttested:
		return "relay_attested"
	default:
		return "unrecognised"
	}
}

// Observation is something a source reported or exposed, at a time and scope. It carries no
// causal authority and is not persisted on its own: it is the input to a candidate.
type Observation struct {
	// SourceObservedAt is the source's own clock. Kept apart from ReceivedAt because collapsing
	// them makes a delayed delivery indistinguishable from a delayed failure, and an investigator
	// reasons about ordering.
	SourceObservedAt time.Time
	ReceivedAt       time.Time
	Statement        string
	Content          string
}

// Candidate is a potential factual claim extracted from a bounded result, before validation. It
// becomes an EvidenceItem or a CoverageGap and never anything else.
type Candidate struct {
	Observation
	CapabilityID      string
	CapabilityVersion uint32
	// Connection is the customer system this came through, and the source an operator filters by.
	Connection uuid.UUID
	// Certificate is the recorded basis on which an absence may be stated as fact. Absent, an
	// absence is a coverage gap rather than evidence. What a customer's redaction policy masked
	// is part of that basis and lives on it — see Certificate.MaskedFields — rather than beside
	// it here, so there is one answer to "may this read support an absence" instead of two that
	// can disagree.
	Certificate *Certificate
	Trust       TrustClass
}

// Certificate is the recorded basis on which an absence may be stated as fact: what scopes were
// searched, how fresh the source was, whether pagination completed, and whether the read was
// fully authorised.
//
// It exists so that "nothing was found" and "nothing was looked at" cannot be told apart by
// accident. A read that was truncated, bounded or partially authorised produces no certificate,
// so no absence claim can rest on it.
type Certificate struct {
	SearchedScope string
	// PaginationComplete is whether the read finished rather than stopping at a bound.
	PaginationComplete bool
	// FullyAuthorized is whether every part of the scope was readable. A partially authorised
	// read that found nothing found nothing it was allowed to see.
	FullyAuthorized bool
	// SourceFreshness is how far back the source itself can answer for. A read whose window
	// extends past it did not search the window that was asked for.
	SourceFreshness time.Duration
	// MaskedFields names what this organization's own redaction policy withheld inside the scope
	// this certificate covers. It is part of the basis rather than a note beside it: the platform
	// did not see those values, so it cannot say what was or was not in them.
	MaskedFields []string
	AttestedBy   TrustClass
}

// Certifies reports whether this basis supports stating an absence as fact.
//
// Masking defeats it for the same reason truncation and partial authorization do. In each case
// part of the scope was not seen, and an absence claim over a scope that was not fully seen is
// the certified negative this whole mechanism exists to prevent — the only difference is that
// here the customer's own policy is what stopped the looking, which makes the mistake more
// tempting rather than less serious.
func (c *Certificate) Certifies() bool {
	return c != nil && c.PaginationComplete && c.FullyAuthorized && len(c.MaskedFields) == 0
}

// masked is what this basis says was withheld, nil-safe so a candidate carrying no certificate
// answers the question the same way as one carrying an unmasked read.
func (c *Certificate) masked() []string {
	if c == nil {
		return nil
	}
	return c.MaskedFields
}

// ErrEvidence reports a candidate that could not become an EvidenceItem.
var ErrEvidence = errors.New("evidence candidate is not admissible")

// Item is an immutable validated factual claim with provenance and an incident-time snapshot. It
// may support or contradict a hypothesis; it never establishes cause on its own.
type Item struct {
	ID      uuid.UUID
	RoundID uuid.UUID
	// Ordinal orders items within a case, stably, so a paginated section keeps its order while
	// the case is still growing.
	Ordinal int
	// RequestID is the read that produced this item, so an engineer reviewing afterwards can go
	// from an item to what was asked for and why it was asked.
	RequestID         uuid.UUID
	CapabilityID      string
	CapabilityVersion uint32
	Connection        uuid.UUID
	// Statement is the factual claim, short and structured enough to read at 03:00.
	Statement string
	// Content is the bounded evidence text this claim rests on. It is fetched separately from a
	// listing, so a listing is not the size of its contents.
	Content string
	// Absence marks an item whose claim is that nothing was there. It requires a certificate;
	// without one the read produced a CoverageGap instead.
	Absence bool
	Trust   TrustClass
	// Certificate is the basis for an absence, and is carried on every item so a reader can see
	// what the read actually covered rather than inferring it.
	Certificate *Certificate
	// SourceObservedAt is when the SOURCE says this happened; zero when the source gave no
	// defensible time, in which case the item is listed beside the timeline rather than placed on
	// it.
	SourceObservedAt time.Time
	ReceivedAt       time.Time
}

// OnTimeline reports whether this item carries a defensible source time. An item without one is
// not placed on the timeline; it is listed beside it, because inventing an ordering for it would
// make the timeline a guess.
func (i Item) OnTimeline() bool { return !i.SourceObservedAt.IsZero() }

// Validated is what one validated capability result produced: the items admitted and the gaps
// that admitting them revealed. Both halves come back together because a result that produced
// no item almost always produced a gap, and returning them apart is how the gap gets dropped.
type Validated struct {
	Items []Item
	Gaps  []Gap
	// Resource is the identity this read established as the cluster reports it, when it
	// established one. Only the workload read does.
	Resource *ResourceIdentity
	// Changes and Topology are what this read contributes to the brief. Each names the item it
	// rests on by its POSITION in Items rather than by identity, because identities are assigned
	// when the items are recorded — which is after this. The runner resolves them once they exist,
	// so nothing on the brief is uncited and nothing here has to guess an identifier.
	Changes  []BriefChange
	Topology []BriefTopology
}

// BriefChange is one change this read observed, before the item behind it has an identity.
type BriefChange struct {
	At      time.Time
	Summary string
	Item    int
}

// BriefTopology is one live relationship this read observed, before the item behind it has an
// identity.
type BriefTopology struct {
	Pod   string
	Node  string
	Owner string
	Phase string
	Ready bool
	Item  int
}

// Validate checks a candidate's grounding, provenance, scope and completeness, and decides
// whether and how it may be used. This is EvidenceValidation: a result never becomes an item
// directly, and the check is one function so there is one place the rules live.
func Validate(candidate Candidate, window Window) (Item, error) {
	switch {
	case candidate.Statement == "":
		return Item{}, fmt.Errorf("%w: it states nothing", ErrEvidence)
	case len(candidate.Statement) > MaxEvidenceStatementBytes:
		return Item{}, fmt.Errorf("%w: the statement is longer than %d bytes",
			ErrEvidence, MaxEvidenceStatementBytes)
	case len(candidate.Content) > MaxEvidenceContentBytes:
		return Item{}, fmt.Errorf("%w: the content is longer than %d bytes",
			ErrEvidence, MaxEvidenceContentBytes)
	case candidate.CapabilityID == "":
		return Item{}, fmt.Errorf("%w: it names no capability", ErrEvidence)
	case candidate.Connection == uuid.Nil:
		return Item{}, fmt.Errorf("%w: it names no connection", ErrEvidence)
	case candidate.Trust == 0:
		return Item{}, fmt.Errorf("%w: it carries no trust class", ErrEvidence)
	}

	// A source-observed time outside the investigation's window is kept, but not as a timeline
	// fact: it belongs to a moment this case is not looking at, and placing it would move the
	// ordering the whole timeline is read for.
	observed := candidate.SourceObservedAt
	if !observed.IsZero() && !window.Covers(observed) {
		observed = time.Time{}
	}

	return Item{
		ID:                uuid.New(),
		CapabilityID:      candidate.CapabilityID,
		CapabilityVersion: candidate.CapabilityVersion,
		Connection:        candidate.Connection,
		Statement:         candidate.Statement,
		Content:           candidate.Content,
		Trust:             candidate.Trust,
		Certificate:       candidate.Certificate,
		SourceObservedAt:  observed,
		ReceivedAt:        candidate.ReceivedAt,
	}, nil
}

// ValidateAbsence is Validate for a candidate whose claim is that nothing was there.
//
// It refuses without a certificate, and that refusal is the whole point: a read that was
// truncated, bounded or partially authorised cannot support an absence claim, and the convenient
// version of this function — admit it and note the doubt — is how an RBAC misconfiguration
// becomes a certified negative.
func ValidateAbsence(candidate Candidate, window Window) (Item, error) {
	if masked := candidate.Certificate.masked(); len(masked) > 0 {
		return Item{}, fmt.Errorf(
			"%w: this organization's redaction policy masked %s in this read, so its absence "+
				"cannot be stated as fact", ErrEvidence, strings.Join(masked, ", "))
	}
	if !candidate.Certificate.Certifies() {
		return Item{}, fmt.Errorf(
			"%w: an absence needs a completeness certificate and this read has none", ErrEvidence)
	}
	item, err := Validate(candidate, window)
	if err != nil {
		return Item{}, err
	}
	item.Absence = true
	return item, nil
}

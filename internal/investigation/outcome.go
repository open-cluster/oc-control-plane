package investigation

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OutcomeKind is what a round concluded, and there are three. A confident conclusion without
// sufficient support is not among them, which is ADR-011 given teeth rather than described.
//
// The values are persisted and frozen by a gate in internal/gates.
type OutcomeKind int16

const (
	// OutcomeSupported is the most supported explanation, surviving alternatives and
	// contradictions. Deliberately not called a cause.
	OutcomeSupported OutcomeKind = iota + 1
	// OutcomeCaveated is an explanation whose support is real and whose coverage is not: it
	// stands, and a gap that could overturn it is named beside it.
	OutcomeCaveated
	// OutcomeAbstained states that no explanation is sufficiently supported, with the missing
	// evidence and contradictions that prevented one. A first-class result, not a failure.
	OutcomeAbstained
)

func (k OutcomeKind) String() string {
	switch k {
	case OutcomeSupported:
		return "supported"
	case OutcomeCaveated:
		return "caveated"
	case OutcomeAbstained:
		return "abstained"
	default:
		return "unrecognised"
	}
}

// ClaimRole is what one cited claim does in an outcome.
//
// The values are persisted and frozen by a gate in internal/gates.
type ClaimRole int16

const (
	// ClaimSupporting is a claim the explanation rests on.
	ClaimSupporting ClaimRole = iota + 1
	// ClaimContradicting is a claim the explanation had to argue past. Retained and shown
	// rather than resolved silently in favour of the leading hypothesis.
	ClaimContradicting
	// ClaimAffectedScope is a statement about what is affected. It carries evidence identifiers
	// like every other claim, which is what stops a figure appearing on the page uncited.
	ClaimAffectedScope
)

func (r ClaimRole) String() string {
	switch r {
	case ClaimSupporting:
		return "supporting"
	case ClaimContradicting:
		return "contradicting"
	case ClaimAffectedScope:
		return "affected_scope"
	default:
		return "unrecognised"
	}
}

// ErrUncited reports a claim with no evidence behind it. It is returned by validation before
// anything is persisted, so citation is a structural property of the output rather than a review
// obligation — an uncited claim is IMPOSSIBLE rather than discouraged.
var ErrUncited = errors.New("claim cites no evidence")

// ErrOutcome reports an outcome that could not be recorded.
var ErrOutcome = errors.New("outcome is not admissible")

// Claim is one statement in an outcome together with the EvidenceItems supporting it. The
// citation is a list of identifiers rather than prose, because flattening a claim to prose is
// what turns an inspectable artifact into an assertable one.
type Claim struct {
	ID        uuid.UUID
	Ordinal   int
	Role      ClaimRole
	Statement string
	// Evidence is the items this claim rests on. At least one, always.
	Evidence []uuid.UUID
}

// Outcome is what a round reached, together with the basis it rests on.
//
// There is deliberately no score anywhere in it. No confidence number, no coverage percentage, no
// calibrated probability: the basis is supporting claims, contradicting claims, relevant
// checks not made, independent sources, and the reasons alternatives were set aside. A number
// here would be read as a verdict and would move the calibration burden onto whoever is awake at
// 03:00.
type Outcome struct {
	ID      uuid.UUID
	RoundID uuid.UUID
	// Round is the ordinal of the round that reached it, carried so a superseded outcome stays
	// attributable without a join.
	Round int
	Kind  OutcomeKind
	// Statement is the explanation, or — for an abstention — what could not be established.
	Statement string
	Claims    []Claim
	// UnresolvedHypotheses are the ones still live when this ended. An abstention with none of
	// these and no gaps is an abstention with no explanation of why, which is a defect.
	UnresolvedHypotheses []uuid.UUID
	// RelevantGaps are the coverage gaps that mattered to this outcome.
	RelevantGaps []uuid.UUID
	// IndependentSources is how many distinct Connections the supporting evidence came through.
	// It is a count of sources and not a score: one source agreeing with itself twice is not two
	// sources, and a reader can see that here.
	IndependentSources int
	// Superseded marks an outcome a later round replaced. It stays readable, attributed and
	// ordered rather than being rewritten (ADR-013).
	Superseded bool
	ReachedAt  time.Time
}

// Draft is what the reasoner emitted, before it has been checked. It is a separate type from
// Outcome for the same reason a Proposal is separate from a Request: giving them one type is how
// something unchecked reaches storage.
type Draft struct {
	Kind      OutcomeKind
	Statement string
	Claims    []DraftClaim
	// Unresolved names hypotheses still live, by their ordinal in what the reasoner was given.
	// Ordinals rather than identifiers because a model must never be in a position to name a row.
	Unresolved []int
	// RelevantGaps names gaps that mattered, by ordinal, for the same reason.
	RelevantGaps []int
}

// DraftClaim is one claim before checking. Evidence is named by ordinal, so a reasoner cannot
// cite an identifier it invented — an invented UUID would look like a citation until someone
// followed it.
type DraftClaim struct {
	Role      ClaimRole
	Statement string
	Evidence  []int
}

// AdmitOutcome turns a reasoner's draft into an outcome, refusing everything that could not be
// checked. It is the output schema, and it runs before storage: a response containing an uncited
// claim does not reach the database.
//
// evidence and gaps are what the reasoner was shown, in the order it was shown them. An ordinal
// outside those bounds is a citation of something that does not exist, and it is refused for the
// same reason an empty citation is.
func AdmitOutcome(draft Draft, evidence []Item, gaps []Gap, live []Hypothesis) (Outcome, error) {
	if draft.Kind == 0 {
		return Outcome{}, fmt.Errorf("%w: it names no kind", ErrOutcome)
	}
	if draft.Statement == "" {
		return Outcome{}, fmt.Errorf("%w: it states nothing", ErrOutcome)
	}

	claims := make([]Claim, 0, len(draft.Claims))
	for position, drafted := range draft.Claims {
		claim, err := admitClaim(position, drafted, evidence)
		if err != nil {
			return Outcome{}, err
		}
		claims = append(claims, claim)
	}

	unresolved, err := resolveOrdinals("hypothesis", draft.Unresolved, len(live))
	if err != nil {
		return Outcome{}, err
	}
	relevant, err := resolveOrdinals("coverage gap", draft.RelevantGaps, len(gaps))
	if err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{
		ID:        uuid.New(),
		Kind:      draft.Kind,
		Statement: draft.Statement,
		Claims:    claims,
	}
	for _, position := range unresolved {
		outcome.UnresolvedHypotheses = append(outcome.UnresolvedHypotheses, live[position].ID)
	}
	for _, position := range relevant {
		outcome.RelevantGaps = append(outcome.RelevantGaps, gaps[position].ID)
	}
	outcome.IndependentSources = independentSources(claims, evidence)

	// An abstention that says nothing about why it abstained is a defect rather than a cautious
	// answer. It has to name at least one thing: a gap that mattered, a hypothesis left
	// unresolved, or a contradiction it could not settle.
	if draft.Kind == OutcomeAbstained && len(outcome.RelevantGaps) == 0 &&
		len(outcome.UnresolvedHypotheses) == 0 && !hasRole(claims, ClaimContradicting) {
		return Outcome{}, fmt.Errorf(
			"%w: an abstention must name what was missing, what was unresolved, or what "+
				"contradicted what", ErrOutcome)
	}

	// A supported explanation with nothing behind it is the outcome ADR-011 exists to make
	// impossible. Caveated and abstained may rest on contradictions and gaps; supported may not
	// rest on nothing.
	if draft.Kind == OutcomeSupported && !hasRole(claims, ClaimSupporting) {
		return Outcome{}, fmt.Errorf("%w: a supported explanation must carry a supporting claim",
			ErrOutcome)
	}
	return outcome, nil
}

func admitClaim(position int, drafted DraftClaim, evidence []Item) (Claim, error) {
	if drafted.Role == 0 {
		return Claim{}, fmt.Errorf("%w: claim %d names no role", ErrOutcome, position+1)
	}
	if drafted.Statement == "" {
		return Claim{}, fmt.Errorf("%w: claim %d states nothing", ErrOutcome, position+1)
	}
	if len(drafted.Evidence) == 0 {
		return Claim{}, fmt.Errorf("%w: claim %d (%q)", ErrUncited, position+1, drafted.Statement)
	}

	cited, err := resolveOrdinals("evidence item", drafted.Evidence, len(evidence))
	if err != nil {
		return Claim{}, err
	}
	claim := Claim{
		ID:        uuid.New(),
		Ordinal:   position + 1,
		Role:      drafted.Role,
		Statement: drafted.Statement,
		Evidence:  make([]uuid.UUID, 0, len(cited)),
	}
	for _, at := range cited {
		claim.Evidence = append(claim.Evidence, evidence[at].ID)
	}
	return claim, nil
}

// resolveOrdinals turns one-based ordinals into zero-based positions, refusing any that names
// something outside what the reasoner was shown. Duplicates are dropped rather than refused: a
// claim citing the same item twice cites it once, and refusing that would be pedantry with a
// retry attached.
func resolveOrdinals(what string, ordinals []int, available int) ([]int, error) {
	seen := make(map[int]struct{}, len(ordinals))
	positions := make([]int, 0, len(ordinals))
	for _, ordinal := range ordinals {
		if ordinal < 1 || ordinal > available {
			return nil, fmt.Errorf("%w: %s %d was never shown to the reasoner",
				ErrOutcome, what, ordinal)
		}
		at := ordinal - 1
		if _, duplicate := seen[at]; duplicate {
			continue
		}
		seen[at] = struct{}{}
		positions = append(positions, at)
	}
	return positions, nil
}

// independentSources counts the distinct Connections the supporting evidence came through. One
// source agreeing with itself is one source, and this is the field that says so.
func independentSources(claims []Claim, evidence []Item) int {
	byID := make(map[uuid.UUID]Item, len(evidence))
	for _, item := range evidence {
		byID[item.ID] = item
	}
	sources := make(map[uuid.UUID]struct{})
	for _, claim := range claims {
		if claim.Role != ClaimSupporting {
			continue
		}
		for _, cited := range claim.Evidence {
			if item, ok := byID[cited]; ok {
				sources[item.Connection] = struct{}{}
			}
		}
	}
	return len(sources)
}

func hasRole(claims []Claim, role ClaimRole) bool {
	for _, claim := range claims {
		if claim.Role == role {
			return true
		}
	}
	return false
}

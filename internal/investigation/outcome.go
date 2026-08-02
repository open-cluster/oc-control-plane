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

// ErrUntraced reports an explanation that corresponds to no hypothesis the investigator proposed
// and settled as supported.
//
// It is told apart from every other refusal because it is the one that says the falsification
// machinery contributed nothing: the claims cited evidence, the schema was satisfied, and the
// stated cause was still something nobody put forward and nothing was read to disprove. An
// operator reading the case has to be able to tell that from "the reasoner could not cite its
// claims", because they call for different things.
var ErrUntraced = errors.New("the explanation traces to no supported hypothesis")

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
	// Explains is the Hypothesis this outcome is the explanation of. Zero on an abstention and
	// required on the other two kinds: an explanation that corresponds to nothing the investigator
	// proposed and tested is a conclusion rather than a finding, and the record cannot show a
	// reader which alternatives it beat.
	Explains uuid.UUID
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
	// Explains names the hypothesis this outcome is the explanation of, by its ordinal among those
	// the reasoner was shown. Exactly one: an explanation resting on two hypotheses at once is two
	// explanations, and choosing between them is the work being asked for. Zero on an abstention.
	Explains int
	// Unresolved names hypotheses still live, by their ordinal in what the reasoner was given.
	// Ordinals rather than identifiers because a model must never be in a position to name a row.
	Unresolved []int
	// RelevantGaps names gaps that mattered, by ordinal, for the same reason.
	RelevantGaps []int
}

// Shown is what the reasoner was given, in the order the ordinals it answers with refer to.
//
// It carries every hypothesis the round holds rather than only the live ones. Passing the live
// subset is what made HypothesisSupported invisible to admission — the one state that decides
// whether an explanation is traceable was also the one state that removed a hypothesis from the
// list admission checked against.
type Shown struct {
	Evidence   []Item
	Gaps       []Gap
	Hypotheses []Hypothesis
	// Tested names the hypotheses at least one DISPATCHED read pointed at. Dispatched rather than
	// proposed: a read refused before it left this control plane put nothing at risk, and a read
	// that failed still did, because the failure is itself in the record.
	Tested map[uuid.UUID]struct{}
}

// Admitted is a draft that passed the output schema, together with what admission had to change
// about it.
type Admitted struct {
	Outcome Outcome
	// Untested reports an explanation resting on a hypothesis no dispatched read pointed at. The
	// caller records the coverage gap, because only the caller can give it an identity — and it
	// must, or the caveat stands on the page with nothing saying why.
	Untested bool
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
// shown is what the reasoner was given, in the order it was given them. An ordinal outside those
// bounds is a citation of something that does not exist, and it is refused for the same reason an
// empty citation is.
func AdmitOutcome(draft Draft, shown Shown) (Admitted, error) {
	if draft.Kind == 0 {
		return Admitted{}, fmt.Errorf("%w: it names no kind", ErrOutcome)
	}
	if draft.Statement == "" {
		return Admitted{}, fmt.Errorf("%w: it states nothing", ErrOutcome)
	}

	claims := make([]Claim, 0, len(draft.Claims))
	for position, drafted := range draft.Claims {
		claim, err := admitClaim(position, drafted, shown.Evidence)
		if err != nil {
			return Admitted{}, err
		}
		claims = append(claims, claim)
	}

	unresolved, err := resolveOrdinals("hypothesis", draft.Unresolved, len(shown.Hypotheses))
	if err != nil {
		return Admitted{}, err
	}
	relevant, err := resolveOrdinals("coverage gap", draft.RelevantGaps, len(shown.Gaps))
	if err != nil {
		return Admitted{}, err
	}

	outcome := Outcome{
		ID:        uuid.New(),
		Kind:      draft.Kind,
		Statement: draft.Statement,
		Claims:    claims,
	}
	for _, position := range unresolved {
		if shown.Hypotheses[position].State != HypothesisLive {
			// A hypothesis this same answer settled is not unresolved. Refusing the contradiction
			// would lose a sound conclusion over bookkeeping the settling already resolved.
			continue
		}
		outcome.UnresolvedHypotheses = append(
			outcome.UnresolvedHypotheses, shown.Hypotheses[position].ID)
	}
	for _, position := range relevant {
		outcome.RelevantGaps = append(outcome.RelevantGaps, shown.Gaps[position].ID)
	}
	outcome.IndependentSources = independentSources(claims, shown.Evidence)

	// An abstention that says nothing about why it abstained is a defect rather than a cautious
	// answer. It has to name at least one thing: a gap that mattered, a hypothesis left
	// unresolved, or a contradiction it could not settle.
	if draft.Kind == OutcomeAbstained && len(outcome.RelevantGaps) == 0 &&
		len(outcome.UnresolvedHypotheses) == 0 && !hasRole(claims, ClaimContradicting) {
		return Admitted{}, fmt.Errorf(
			"%w: an abstention must name what was missing, what was unresolved, or what "+
				"contradicted what", ErrOutcome)
	}

	// A supported explanation with nothing behind it is the outcome ADR-011 exists to make
	// impossible. Caveated and abstained may rest on contradictions and gaps; supported may not
	// rest on nothing. Checked against the DRAFTED kind, so a draft demoted below cannot pass by
	// being demoted.
	if draft.Kind == OutcomeSupported && !hasRole(claims, ClaimSupporting) {
		return Admitted{}, fmt.Errorf("%w: a supported explanation must carry a supporting claim",
			ErrOutcome)
	}

	return trace(draft, outcome, shown)
}

// trace binds the explanation to the hypothesis it came from, and decides what an explanation
// nothing was read to disprove is.
//
// This is the standard that says the hypothesis record is the reasoning rather than decoration
// beside a conclusion. Without it a round can falsify every alternative it proposed and still state
// a cause nobody put forward, and the case file looks the same either way.
func trace(draft Draft, outcome Outcome, shown Shown) (Admitted, error) {
	if draft.Kind == OutcomeAbstained {
		// An abstention says no explanation was sufficiently supported. Naming the hypothesis it
		// explains would contradict its own kind, so it is refused rather than silently ignored.
		if draft.Explains != 0 {
			return Admitted{}, fmt.Errorf(
				"%w: an abstention states no explanation, so it explains no hypothesis", ErrOutcome)
		}
		return Admitted{Outcome: outcome}, nil
	}

	if draft.Explains == 0 {
		return Admitted{}, fmt.Errorf(
			"%w: it states an explanation without naming the hypothesis that explanation is",
			ErrUntraced)
	}
	if draft.Explains < 1 || draft.Explains > len(shown.Hypotheses) {
		return Admitted{}, fmt.Errorf("%w: hypothesis %d was never shown to the reasoner",
			ErrOutcome, draft.Explains)
	}

	explains := shown.Hypotheses[draft.Explains-1]
	if explains.State != HypothesisSupported {
		return Admitted{}, fmt.Errorf(
			"%w: hypothesis %d is %s, and an explanation may only be a hypothesis the evidence "+
				"supported", ErrUntraced, draft.Explains, explains.State)
	}
	outcome.Explains = explains.ID

	// A hypothesis no dispatched read pointed at was never put at risk: its falsification
	// condition selected nothing, and the evidence behind it was gathered for other reasons. That
	// is a real explanation and a weaker one, and the two have to be told apart or "supported"
	// means both. It is decided here from what this control plane actually sent, never asked of
	// the reasoner, because a reasoner grading its own rigour grades it generously.
	if _, put := shown.Tested[explains.ID]; put {
		return Admitted{Outcome: outcome}, nil
	}
	if outcome.Kind == OutcomeSupported {
		outcome.Kind = OutcomeCaveated
	}
	return Admitted{Outcome: outcome, Untested: true}, nil
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

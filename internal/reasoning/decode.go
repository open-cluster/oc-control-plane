package reasoning

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// TURNING A DOCUMENT INTO DOMAIN VALUES, AND REFUSING EVERYTHING THAT COULD NOT BE CHECKED.
//
// This is where the boundary's invariants are enforced for every provider equally. The schemas make
// the wrong shapes unstateable, but a provider that cannot enforce a schema will occasionally
// return one anyway — so nothing below trusts that the document is well formed, and an ordinal
// outside what the reasoner was shown is refused before anything is returned.
//
// Bounds live here rather than in the schemas because several providers silently drop schema
// bounds, and a bound that is silently dropped is a bound nobody has.

// What one answer may contain. The numbers are deliberately small: this boundary produces a
// reviewable artifact, and a document larger than these is not one a person is going to read.
const (
	maxHypotheses     = 8
	maxProposals      = 8
	maxClaims         = 24
	maxWeighings      = 64
	maxSettlings      = 16
	maxStatementBytes = 2000
)

// hypothesis is one proposed explanation as it arrives, at any of the three calls.
type hypothesis struct {
	Statement string `json:"statement"`
	Falsifies string `json:"falsifies"`
}

// decodeHypotheses turns the opening document into the hypotheses the planner proposed.
func decodeHypotheses(document []byte) ([]investigation.Hypothesis, error) {
	var answer struct {
		Hypotheses []hypothesis `json:"hypotheses"`
	}
	if err := unmarshal(document, &answer); err != nil {
		return nil, err
	}
	if len(answer.Hypotheses) == 0 {
		return nil, malformed("it proposed no hypotheses, and an opening with none is not an " +
			"investigation")
	}
	// Nothing is held yet, so a proposal here can only restate another in this same document, and
	// one that does is dropped exactly as it would be later. The resolver is discarded because no
	// ordinal in the opening document refers to a hypothesis: there is nothing yet to point at.
	proposed, _, err := decodeProposedHypotheses(answer.Hypotheses, nil)
	return proposed, err
}

// decodeProposedHypotheses reads hypotheses from any of the three documents, and returns the ones
// worth appending together with the map from the ordinals the reasoner USED to the ordinals the
// round will actually hold.
//
// A reasoner shown a place to put hypotheses fills it with the whole list it is holding rather than
// only what is new. It did that on a live run and left a case carrying the same explanation twice,
// in the supported state both times. Refusing the document would be the expensive answer — a refused
// document costs a retry and then the round — so a restatement is DROPPED and every ordinal pointing
// at it is redirected to the hypothesis it restates. That is what the reasoner meant by it, and
// dropping a duplicate rather than refusing it is what this package already does with a claim that
// cites the same evidence twice.
//
// The remapping is here, once, because this is the only place both lists are in view. The ordinals
// the domain receives are the ordinals of the list the domain holds, and nothing downstream has to
// know a restatement was ever sent.
func decodeProposedHypotheses(
	proposals []hypothesis, shown []investigation.Hypothesis,
) ([]investigation.Hypothesis, func(int) int, error) {
	held := len(shown)
	if len(proposals) == 0 {
		return nil, identity, nil
	}
	if held+len(proposals) > maxHypotheses {
		return nil, nil, malformed(
			"it would leave %d hypotheses in the round and at most %d are usable",
			held+len(proposals), maxHypotheses)
	}

	// Where a statement already sits, by its ordinal. Compared on the statement alone: a reasoner
	// restating an explanation rarely restates the falsification condition word for word, and
	// treating those as different hypotheses is the duplicate this exists to prevent.
	at := make(map[string]int, held+len(proposals))
	for index, hypothesis := range shown {
		at[fold(hypothesis.Statement)] = index + 1
	}

	moved := make(map[int]int, len(proposals))
	proposed := make([]investigation.Hypothesis, 0, len(proposals))
	for position, proposal := range proposals {
		used := held + position + 1
		statement := oneLine(proposal.Statement)
		falsifies := oneLine(proposal.Falsifies)
		switch {
		case statement == "":
			return nil, nil, malformed("hypothesis %d states nothing", used)
		case falsifies == "":
			// Required rather than optional: an explanation nothing could disprove is a belief,
			// and telling those apart is what the rest of this system is for.
			return nil, nil, malformed(
				"hypothesis %d names nothing that would disprove it", used)
		case len(statement) > maxStatementBytes || len(falsifies) > maxStatementBytes:
			return nil, nil, malformed("hypothesis %d is longer than %d bytes",
				used, maxStatementBytes)
		}

		if existing, restated := at[fold(statement)]; restated {
			moved[used] = existing
			continue
		}
		ordinal := held + len(proposed) + 1
		at[fold(statement)] = ordinal
		moved[used] = ordinal
		proposed = append(proposed, investigation.Hypothesis{
			Ordinal:   ordinal,
			Statement: statement,
			Falsifies: falsifies,
			State:     investigation.HypothesisLive,
		})
	}

	return proposed, func(ordinal int) int {
		if to, remapped := moved[ordinal]; remapped {
			return to
		}
		return ordinal
	}, nil
}

// identity is the resolver for a document that proposed nothing, so a caller never branches on
// whether remapping happened.
func identity(ordinal int) int { return ordinal }

// fold is how two statements are compared for being the same explanation. Case and surrounding
// space are not a difference a reader would call one, and a reasoner restating itself rarely
// reproduces its own capitalisation.
func fold(statement string) string {
	return strings.ToLower(strings.TrimSpace(oneLine(statement)))
}

// decodeProposals turns the planning document into reads, weighings and settlings.
//
// The reads it produces are structurally inside the investigation's scope: the namespace, the
// workload and the window are filled in from the case rather than read from the answer, and a pod
// is resolved from the ordinal the model gave against the pods the brief actually found. A read of
// another namespace, another workload or a pod nobody resolved cannot be expressed here, and is
// refused again by validation before dispatch.
func decodeProposals(
	document []byte, deliberation investigation.Deliberation,
) (investigation.Proposed, error) {
	var answer struct {
		Proposals []struct {
			Capability    string `json:"capability"`
			Justification int    `json:"justification"`
			Reason        string `json:"reason"`
			Arguments     struct {
				Pod       int    `json:"pod"`
				Container string `json:"container"`
				Previous  bool   `json:"previous"`
				MaxPods   int    `json:"max_pods"`
				MaxEvents int    `json:"max_events"`
				MaxLines  int    `json:"max_lines"`
			} `json:"arguments"`
		} `json:"proposals"`
		Hypotheses []hypothesis `json:"hypotheses"`
		Weighings  []weighing   `json:"weighings"`
		Settlings  []settling   `json:"settlings"`
	}
	if err := unmarshal(document, &answer); err != nil {
		return investigation.Proposed{}, err
	}
	if len(answer.Proposals) > maxProposals {
		return investigation.Proposed{}, malformed(
			"it proposed %d reads and at most %d are usable", len(answer.Proposals), maxProposals)
	}

	// Decoded before the reads, because a read may point at a hypothesis this same answer proposed.
	// The prompt invites exactly that — discover a cause, then ask for what would disprove it — and
	// bounding the justification by what the reasoner was SHOWN would refuse the whole document for
	// following the instruction it was given. The runner appends these before it validates the
	// reads, so the ordinal resolves there too.
	proposedHypotheses, resolve, err := decodeProposedHypotheses(
		answer.Hypotheses, deliberation.Hypotheses)
	if err != nil {
		return investigation.Proposed{}, err
	}
	// Bounds are checked against the list the reasoner ANSWERED in, which still counts anything it
	// restated; the resolver then moves each ordinal onto the list the round will hold. Checking
	// against the shorter list would refuse a document for naming a hypothesis it had just sent.
	used := len(deliberation.Hypotheses) + len(answer.Hypotheses)

	scope := scopeOf(deliberation.Brief)
	proposals := make([]investigation.Proposal, 0, len(answer.Proposals))
	for position, proposal := range answer.Proposals {
		version, known := versionOf(deliberation.Available, proposal.Capability)
		if !known {
			return investigation.Proposed{}, malformed(
				"read %d names %q, which is not available in this environment",
				position+1, proposal.Capability)
		}
		if proposal.Justification < 1 || proposal.Justification > used {
			// The containment mechanism, not bookkeeping. A read must point at a typed hypothesis
			// a human can read; that is the chain evidence text may never bypass.
			return investigation.Proposed{}, malformed(
				"read %d points at hypothesis %d, which it was never shown",
				position+1, proposal.Justification)
		}
		reason := oneLine(proposal.Reason)
		if reason == "" {
			return investigation.Proposed{}, malformed(
				"read %d gives no reason it bears on the hypothesis it points at", position+1)
		}

		arguments := investigation.Arguments{
			Namespace:    scope.namespace,
			WorkloadKind: scope.kind,
			WorkloadName: scope.name,
			Window:       deliberation.Brief.Window,
			Previous:     proposal.Arguments.Previous,
			MaxPods:      bound(proposal.Arguments.MaxPods, capability.MaxPods),
			MaxEvents:    bound(proposal.Arguments.MaxEvents, capability.MaxEvents),
			MaxLines:     bound(proposal.Arguments.MaxLines, capability.MaxLines),
		}
		if proposal.Capability == kubernetesContainerLogs {
			pod, resolved := podAt(deliberation.Brief, proposal.Arguments.Pod)
			if !resolved {
				return investigation.Proposed{}, malformed(
					"read %d names pod %d, which is not one this investigation resolved",
					position+1, proposal.Arguments.Pod)
			}
			container := oneLine(proposal.Arguments.Container)
			if container == "" {
				return investigation.Proposed{}, malformed(
					"read %d asks for a container log without naming a container", position+1)
			}
			arguments.PodName = pod
			arguments.ContainerName = container
		}

		proposals = append(proposals, investigation.Proposal{
			CapabilityID:      proposal.Capability,
			CapabilityVersion: version,
			Arguments:         arguments,
			Justification:     resolve(proposal.Justification),
			Reason:            reason,
		})
	}

	weighings, err := decodeWeighings(answer.Weighings, len(deliberation.Evidence), used, resolve)
	if err != nil {
		return investigation.Proposed{}, err
	}
	settlings, err := decodeSettlings(answer.Settlings, used, resolve)
	if err != nil {
		return investigation.Proposed{}, err
	}
	return investigation.Proposed{
		Proposals:  proposals,
		Hypotheses: proposedHypotheses,
		Weighings:  weighings,
		Settlings:  settlings,
	}, nil
}

// decodeConclusion turns the closing document into a draft outcome.
func decodeConclusion(
	document []byte, deliberation investigation.Deliberation,
) (investigation.Concluded, error) {
	var answer struct {
		Kind      string `json:"kind"`
		Statement string `json:"statement"`
		Explains  int    `json:"explains"`
		Claims    []struct {
			Role      string `json:"role"`
			Statement string `json:"statement"`
			Evidence  []int  `json:"evidence"`
		} `json:"claims"`
		Hypotheses   []hypothesis `json:"hypotheses"`
		Unresolved   []int        `json:"unresolved"`
		RelevantGaps []int        `json:"relevant_gaps"`
		Weighings    []weighing   `json:"weighings"`
		Settlings    []settling   `json:"settlings"`
	}
	if err := unmarshal(document, &answer); err != nil {
		return investigation.Concluded{}, err
	}

	kind, known := outcomeKind(answer.Kind)
	if !known {
		return investigation.Concluded{}, malformed(
			"it names outcome kind %q, which is not one of supported, caveated or abstained",
			answer.Kind)
	}
	statement := oneLine(answer.Statement)
	if statement == "" {
		return investigation.Concluded{}, malformed("it states nothing")
	}
	if len(answer.Claims) > maxClaims {
		return investigation.Concluded{}, malformed(
			"it makes %d claims and at most %d are usable", len(answer.Claims), maxClaims)
	}

	claims := make([]investigation.DraftClaim, 0, len(answer.Claims))
	for position, claim := range answer.Claims {
		role, roleKnown := claimRole(claim.Role)
		if !roleKnown {
			return investigation.Concluded{}, malformed(
				"claim %d names role %q, which is not one of supporting, contradicting or "+
					"affected_scope", position+1, claim.Role)
		}
		claimStatement := oneLine(claim.Statement)
		if claimStatement == "" {
			return investigation.Concluded{}, malformed("claim %d states nothing", position+1)
		}
		if len(claim.Evidence) == 0 {
			// An uncited claim is impossible rather than discouraged, and this is where that is
			// true for every provider regardless of what its schema mode enforced.
			return investigation.Concluded{}, malformed(
				"claim %d (%q) cites no evidence", position+1, claimStatement)
		}
		for _, ordinal := range claim.Evidence {
			if ordinal < 1 || ordinal > len(deliberation.Evidence) {
				return investigation.Concluded{}, malformed(
					"claim %d cites evidence %d, which it was never shown", position+1, ordinal)
			}
		}
		claims = append(claims, investigation.DraftClaim{
			Role:      role,
			Statement: claimStatement,
			Evidence:  claim.Evidence,
		})
	}

	for _, ordinal := range answer.RelevantGaps {
		if ordinal < 1 || ordinal > len(deliberation.Gaps) {
			return investigation.Concluded{}, malformed(
				"it names coverage gap %d, which it was never shown", ordinal)
		}
	}

	proposed, resolve, err := decodeProposedHypotheses(answer.Hypotheses, deliberation.Hypotheses)
	if err != nil {
		return investigation.Concluded{}, err
	}
	// A hypothesis proposed by this same document is nameable by it. The round appends these before
	// the outcome is admitted, so the ordinals they will hold are known here — and without that, a
	// cause the evidence revealed at the last call could be stated but never traced. Bounds are
	// checked against what the reasoner answered in, which still counts anything it restated; the
	// resolver moves each ordinal onto the list the round will actually hold.
	used := len(deliberation.Hypotheses) + len(answer.Hypotheses)

	weighings, err := decodeWeighings(answer.Weighings, len(deliberation.Evidence), used, resolve)
	if err != nil {
		return investigation.Concluded{}, err
	}
	settlings, err := decodeSettlings(answer.Settlings, used, resolve)
	if err != nil {
		return investigation.Concluded{}, err
	}
	if answer.Explains < 0 || answer.Explains > used {
		return investigation.Concluded{}, malformed(
			"it explains hypothesis %d, which is neither one it was shown nor one it proposed",
			answer.Explains)
	}
	unresolved := make([]int, 0, len(answer.Unresolved))
	for _, ordinal := range answer.Unresolved {
		if ordinal < 1 || ordinal > used {
			return investigation.Concluded{}, malformed(
				"it names hypothesis %d as unresolved, which it was never shown", ordinal)
		}
		unresolved = append(unresolved, resolve(ordinal))
	}

	return investigation.Concluded{
		Draft: investigation.Draft{
			Kind:         kind,
			Statement:    statement,
			Explains:     resolve(answer.Explains),
			Claims:       claims,
			Unresolved:   unresolved,
			RelevantGaps: answer.RelevantGaps,
		},
		Hypotheses: proposed,
		Weighings:  weighings,
		Settlings:  settlings,
	}, nil
}

// weighing and settling are shared by two documents, so they are decoded once.
type weighing struct {
	Hypothesis int    `json:"hypothesis"`
	Evidence   int    `json:"evidence"`
	Stance     string `json:"stance"`
	Reason     string `json:"reason"`
}

type settling struct {
	Hypothesis int    `json:"hypothesis"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
}

// decodeWeighings reads how evidence stands towards hypotheses.
//
// holding counts what the round will hold once this answer's proposals are appended, for the same
// reason decodeSettlings does: a reasoner that discovered a hypothesis FROM the evidence has to be
// able to record how that evidence stands towards it, and that record is most of what shows the
// discovery was reasoning rather than assertion.
func decodeWeighings(
	weighings []weighing, evidence int, used int, resolve func(int) int,
) ([]investigation.Weighing, error) {
	if len(weighings) > maxWeighings {
		return nil, malformed("it weighs %d pairs and at most %d are usable",
			len(weighings), maxWeighings)
	}
	decoded := make([]investigation.Weighing, 0, len(weighings))
	for position, weighed := range weighings {
		if weighed.Hypothesis < 1 || weighed.Hypothesis > used {
			return nil, malformed("weighing %d names hypothesis %d, which it was never shown",
				position+1, weighed.Hypothesis)
		}
		if weighed.Evidence < 1 || weighed.Evidence > evidence {
			return nil, malformed("weighing %d names evidence %d, which it was never shown",
				position+1, weighed.Evidence)
		}
		stance, known := stanceOf(weighed.Stance)
		if !known {
			return nil, malformed(
				"weighing %d names stance %q, which is not one of supports, contradicts or "+
					"neutral", position+1, weighed.Stance)
		}
		reason := oneLine(weighed.Reason)
		if reason == "" {
			// A stance with no reason is an assertion, and an assertion is the thing persisting
			// reasoning artifacts was supposed to replace.
			return nil, malformed("weighing %d gives no reason", position+1)
		}
		decoded = append(decoded, investigation.Weighing{
			Hypothesis: resolve(weighed.Hypothesis),
			Evidence:   weighed.Evidence,
			Stance:     stance,
			Reason:     reason,
		})
	}
	return decoded, nil
}

// decodeSettlings reads which hypotheses have moved.
//
// holding is how many the round will carry once this same answer's proposals are appended, not how
// many the reasoner was shown. A hypothesis proposed by this document may be settled by it — which
// is the whole point of allowing a late proposal, since one that could never be settled could never
// be the explanation either.
func decodeSettlings(
	settlings []settling, used int, resolve func(int) int,
) ([]investigation.Settling, error) {
	if len(settlings) > maxSettlings {
		return nil, malformed("it settles %d hypotheses and at most %d are usable",
			len(settlings), maxSettlings)
	}
	decoded := make([]investigation.Settling, 0, len(settlings))
	for position, settled := range settlings {
		if settled.Hypothesis < 1 || settled.Hypothesis > used {
			return nil, malformed("settling %d names hypothesis %d, which it was never shown",
				position+1, settled.Hypothesis)
		}
		state, known := hypothesisState(settled.State)
		if !known {
			return nil, malformed(
				"settling %d names state %q, which is not one of supported, falsified or "+
					"set_aside", position+1, settled.State)
		}
		reason := oneLine(settled.Reason)
		if state == investigation.HypothesisSetAside && reason == "" {
			return nil, malformed(
				"settling %d sets a hypothesis aside without saying why, and an alternative set "+
					"aside silently is one a reader cannot check", position+1)
		}
		decoded = append(decoded, investigation.Settling{
			Hypothesis: resolve(settled.Hypothesis),
			State:      state,
			Reason:     reason,
		})
	}
	return decoded, nil
}

// scoped is what a read's fixed fields are filled from. They come from the case rather than from
// the answer, which is what makes a proposal structurally in-scope before anything validates it.
type scoped struct {
	namespace string
	name      string
	kind      investigation.WorkloadKind
}

func scopeOf(brief investigation.Brief) scoped {
	kind, _ := investigation.ParseWorkloadKind(brief.Resource.Kind)
	return scoped{
		namespace: brief.Resource.Namespace,
		name:      brief.Resource.Name,
		kind:      kind,
	}
}

// podAt resolves a pod ordinal against the pods the brief actually found. Zero is legitimate for a
// read that is not about one pod; anything outside the list is refused.
func podAt(brief investigation.Brief, ordinal int) (string, bool) {
	if ordinal < 1 || ordinal > len(brief.Topology) {
		return "", false
	}
	pod := brief.Topology[ordinal-1].Pod
	return pod, pod != ""
}

// versionOf reads the capability's schema version from what this environment offers, so a model
// never names a version and a version it might have invented cannot travel.
func versionOf(available []investigation.CapabilityRef, id string) (uint32, bool) {
	for _, ref := range available {
		if ref.ID == id {
			return ref.Version, true
		}
	}
	return 0, false
}

// bound keeps a requested ceiling inside the capability's own. A planner that named nothing gets
// the capability's ceiling rather than zero.
func bound(asked int, ceiling uint32) uint32 {
	if asked <= 0 || uint32(asked) > ceiling {
		return ceiling
	}
	return uint32(asked)
}

func outcomeKind(value string) (investigation.OutcomeKind, bool) {
	switch value {
	case "supported":
		return investigation.OutcomeSupported, true
	case "caveated":
		return investigation.OutcomeCaveated, true
	case "abstained":
		return investigation.OutcomeAbstained, true
	default:
		return 0, false
	}
}

func claimRole(value string) (investigation.ClaimRole, bool) {
	switch value {
	case "supporting":
		return investigation.ClaimSupporting, true
	case "contradicting":
		return investigation.ClaimContradicting, true
	case "affected_scope":
		return investigation.ClaimAffectedScope, true
	default:
		return 0, false
	}
}

func stanceOf(value string) (investigation.Stance, bool) {
	return investigation.ParseStance(value)
}

func hypothesisState(value string) (investigation.HypothesisState, bool) {
	switch value {
	case "supported":
		return investigation.HypothesisSupported, true
	case "falsified":
		return investigation.HypothesisFalsified, true
	case "set_aside":
		return investigation.HypothesisSetAside, true
	default:
		return 0, false
	}
}

// unmarshal refuses a document that is not the shape asked for, including one carrying fields the
// schema never declared. A provider that invented a field has not answered the contract.
func unmarshal(document []byte, into any) error {
	decoder := json.NewDecoder(newTrimmedReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return malformed("it is not the document the schema describes: %s", err.Error())
	}
	return nil
}

// malformed builds the outcome for an answer that did not satisfy the schema. The provider and
// model are filled in by the caller, which is the only place that knows which one answered.
func malformed(format string, arguments ...any) error {
	return &Failure{Outcome: OutcomeMalformed, Detail: fmt.Sprintf(format, arguments...)}
}

// newTrimmedReader unwraps a document a provider fenced in markdown.
//
// A provider that enforces the schema returns bare JSON and this does nothing. A provider whose
// JSON mode is a request rather than a guarantee sometimes returns the same document inside a code
// fence, and refusing that would spend a retry on punctuation. The unwrapping is deliberately
// narrow — a fence at the very start and end, nothing else — because scanning for the first brace
// would let a document be extracted out of surrounding prose, and prose around the answer means
// the model did something other than what was asked.
func newTrimmedReader(document []byte) io.Reader {
	trimmed := bytes.TrimSpace(document)
	if !bytes.HasPrefix(trimmed, fence) {
		return bytes.NewReader(trimmed)
	}
	trimmed = bytes.TrimPrefix(trimmed, fence)
	if newline := bytes.IndexByte(trimmed, '\n'); newline >= 0 {
		// Whatever language the fence was tagged with, on the rest of the opening line.
		trimmed = trimmed[newline+1:]
	}
	if end := bytes.LastIndex(trimmed, fence); end >= 0 {
		trimmed = trimmed[:end]
	}
	return bytes.NewReader(bytes.TrimSpace(trimmed))
}

var fence = []byte("```")

package reasoning

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

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

// decodeHypotheses turns the opening document into the hypotheses the planner proposed.
func decodeHypotheses(document []byte) ([]investigation.Hypothesis, error) {
	var answer struct {
		Hypotheses []struct {
			Statement string `json:"statement"`
			Falsifies string `json:"falsifies"`
		} `json:"hypotheses"`
	}
	if err := unmarshal(document, &answer); err != nil {
		return nil, err
	}
	if len(answer.Hypotheses) == 0 {
		return nil, malformed("it proposed no hypotheses, and an opening with none is not an " +
			"investigation")
	}
	if len(answer.Hypotheses) > maxHypotheses {
		return nil, malformed("it proposed %d hypotheses and at most %d are usable",
			len(answer.Hypotheses), maxHypotheses)
	}

	proposed := make([]investigation.Hypothesis, 0, len(answer.Hypotheses))
	for position, hypothesis := range answer.Hypotheses {
		statement := oneLine(hypothesis.Statement)
		falsifies := oneLine(hypothesis.Falsifies)
		switch {
		case statement == "":
			return nil, malformed("hypothesis %d states nothing", position+1)
		case falsifies == "":
			// Required rather than optional: an explanation nothing could disprove is a belief,
			// and telling those apart is what the rest of this system is for.
			return nil, malformed(
				"hypothesis %d names nothing that would disprove it", position+1)
		case len(statement) > maxStatementBytes || len(falsifies) > maxStatementBytes:
			return nil, malformed("hypothesis %d is longer than %d bytes",
				position+1, maxStatementBytes)
		}
		proposed = append(proposed, investigation.Hypothesis{
			Ordinal:   position + 1,
			Statement: statement,
			Falsifies: falsifies,
			State:     investigation.HypothesisLive,
		})
	}
	return proposed, nil
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
		Weighings []weighing `json:"weighings"`
		Settlings []settling `json:"settlings"`
	}
	if err := unmarshal(document, &answer); err != nil {
		return investigation.Proposed{}, err
	}
	if len(answer.Proposals) > maxProposals {
		return investigation.Proposed{}, malformed(
			"it proposed %d reads and at most %d are usable", len(answer.Proposals), maxProposals)
	}

	scope := scopeOf(deliberation.Brief)
	proposals := make([]investigation.Proposal, 0, len(answer.Proposals))
	for position, proposal := range answer.Proposals {
		version, known := versionOf(deliberation.Available, proposal.Capability)
		if !known {
			return investigation.Proposed{}, malformed(
				"read %d names %q, which is not available in this environment",
				position+1, proposal.Capability)
		}
		if proposal.Justification < 1 || proposal.Justification > len(deliberation.Hypotheses) {
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
			Justification:     proposal.Justification,
			Reason:            reason,
		})
	}

	weighings, err := decodeWeighings(answer.Weighings, deliberation)
	if err != nil {
		return investigation.Proposed{}, err
	}
	settlings, err := decodeSettlings(answer.Settlings, deliberation)
	if err != nil {
		return investigation.Proposed{}, err
	}
	return investigation.Proposed{
		Proposals: proposals,
		Weighings: weighings,
		Settlings: settlings,
	}, nil
}

// decodeConclusion turns the closing document into a draft outcome.
func decodeConclusion(
	document []byte, deliberation investigation.Deliberation,
) (investigation.Concluded, error) {
	var answer struct {
		Kind      string `json:"kind"`
		Statement string `json:"statement"`
		Claims    []struct {
			Role      string `json:"role"`
			Statement string `json:"statement"`
			Evidence  []int  `json:"evidence"`
		} `json:"claims"`
		Unresolved   []int      `json:"unresolved"`
		RelevantGaps []int      `json:"relevant_gaps"`
		Weighings    []weighing `json:"weighings"`
		Settlings    []settling `json:"settlings"`
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

	weighings, err := decodeWeighings(answer.Weighings, deliberation)
	if err != nil {
		return investigation.Concluded{}, err
	}
	settlings, err := decodeSettlings(answer.Settlings, deliberation)
	if err != nil {
		return investigation.Concluded{}, err
	}
	unresolved, err := unresolvedAfterSettling(answer.Unresolved, settlings, deliberation)
	if err != nil {
		return investigation.Concluded{}, err
	}

	return investigation.Concluded{
		Draft: investigation.Draft{
			Kind:         kind,
			Statement:    statement,
			Claims:       claims,
			Unresolved:   unresolved,
			RelevantGaps: answer.RelevantGaps,
		},
		Weighings: weighings,
		Settlings: settlings,
	}, nil
}

// unresolvedAfterSettling translates the hypotheses the reasoner called unresolved into the
// positions the domain will check them against.
//
// The reasoner is shown every hypothesis and answers in those ordinals. The outcome is admitted
// against the hypotheses still LIVE once this same answer's settlings have been applied, which is
// a shorter list with different positions. Handing the first set of numbers to the second list
// would refuse a perfectly good conclusion for naming a hypothesis that exists — so the mapping
// happens here, once, where both lists are in view.
//
// A hypothesis this answer just settled is dropped rather than refused. Calling something both
// settled and unresolved is a contradiction the settling already resolved, and losing a sound
// conclusion over it would be pedantry with a round attached.
func unresolvedAfterSettling(
	named []int, settlings []investigation.Settling, deliberation investigation.Deliberation,
) ([]int, error) {
	states := make([]investigation.HypothesisState, len(deliberation.Hypotheses))
	for index, hypothesis := range deliberation.Hypotheses {
		states[index] = hypothesis.State
	}
	// Mirrors what the runner does with these settlings, including which ones it skips, so the
	// list computed here is the list the outcome is actually admitted against.
	for _, settling := range settlings {
		if settling.Hypothesis < 1 || settling.Hypothesis > len(states) || settling.State == 0 {
			continue
		}
		if settling.State == investigation.HypothesisSetAside && settling.Reason == "" {
			continue
		}
		states[settling.Hypothesis-1] = settling.State
	}

	livePosition := make(map[int]int, len(states))
	live := 0
	for index, state := range states {
		if state == investigation.HypothesisLive {
			live++
			livePosition[index+1] = live
		}
	}

	translated := make([]int, 0, len(named))
	for _, ordinal := range named {
		if ordinal < 1 || ordinal > len(deliberation.Hypotheses) {
			return nil, malformed(
				"it names hypothesis %d as unresolved, which it was never shown", ordinal)
		}
		if position, stillLive := livePosition[ordinal]; stillLive {
			translated = append(translated, position)
		}
	}
	return translated, nil
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

func decodeWeighings(
	weighings []weighing, deliberation investigation.Deliberation,
) ([]investigation.Weighing, error) {
	if len(weighings) > maxWeighings {
		return nil, malformed("it weighs %d pairs and at most %d are usable",
			len(weighings), maxWeighings)
	}
	decoded := make([]investigation.Weighing, 0, len(weighings))
	for position, weighed := range weighings {
		if weighed.Hypothesis < 1 || weighed.Hypothesis > len(deliberation.Hypotheses) {
			return nil, malformed("weighing %d names hypothesis %d, which it was never shown",
				position+1, weighed.Hypothesis)
		}
		if weighed.Evidence < 1 || weighed.Evidence > len(deliberation.Evidence) {
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
			Hypothesis: weighed.Hypothesis,
			Evidence:   weighed.Evidence,
			Stance:     stance,
			Reason:     reason,
		})
	}
	return decoded, nil
}

func decodeSettlings(
	settlings []settling, deliberation investigation.Deliberation,
) ([]investigation.Settling, error) {
	if len(settlings) > maxSettlings {
		return nil, malformed("it settles %d hypotheses and at most %d are usable",
			len(settlings), maxSettlings)
	}
	decoded := make([]investigation.Settling, 0, len(settlings))
	for position, settled := range settlings {
		if settled.Hypothesis < 1 || settled.Hypothesis > len(deliberation.Hypotheses) {
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
			Hypothesis: settled.Hypothesis,
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

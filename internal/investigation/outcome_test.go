package investigation_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The output schema is what makes an uncited claim impossible rather than discouraged, so these
// assert the refusals rather than the happy path. Each one is the shape of a model response that
// must not reach storage.

func items(count int) []investigation.Item {
	made := make([]investigation.Item, 0, count)
	for range count {
		made = append(made, investigation.Item{ID: uuid.New(), Connection: uuid.New()})
	}
	return made
}

// shownWithATestedHypothesis is what the reasoner was shown together with one supported hypothesis
// a dispatched read actually pointed at. It is the ordinary case, so the tests that are about
// something else do not have to construct it.
func shownWithATestedHypothesis(
	evidence []investigation.Item, gaps []investigation.Gap,
) investigation.Shown {
	hypothesis := investigation.Hypothesis{
		ID:        uuid.New(),
		Ordinal:   1,
		Statement: "the container cannot read the configuration it needs",
		Falsifies: "the container starts",
		State:     investigation.HypothesisSupported,
	}
	return investigation.Shown{
		Evidence:   evidence,
		Gaps:       gaps,
		Hypotheses: []investigation.Hypothesis{hypothesis},
		Tested:     map[uuid.UUID]struct{}{hypothesis.ID: {}},
	}
}

func TestAdmitOutcome_RefusesAnUncitedClaim(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the container exits because its configuration names an unreachable host",
		Explains:  1,
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "it exits with code 1"},
		},
	}, shownWithATestedHypothesis(items(2), nil))

	if !errors.Is(err, investigation.ErrUncited) {
		t.Fatalf("a claim citing nothing must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesACitationOfEvidenceThatWasNeverShown(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload is failing",
		Explains:  1,
		Claims: []investigation.DraftClaim{
			// Two items were shown; this cites a third.
			{Role: investigation.ClaimSupporting, Statement: "it exits", Evidence: []int{3}},
		},
	}, shownWithATestedHypothesis(items(2), nil))

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("citing evidence that was never shown must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesASupportedExplanationWithNoSupportingClaim(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload is failing",
		Explains:  1,
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimContradicting, Statement: "the node is healthy",
				Evidence: []int{1}},
		},
	}, shownWithATestedHypothesis(items(2), nil))

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("a supported explanation resting on nothing must be refused, got %v", err)
	}
}

// An abstention with no explanation of why is a defect, so the schema refuses one.
func TestAdmitOutcome_RefusesAnAbstentionThatNamesNothing(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeAbstained,
		Statement: "no explanation is sufficiently supported",
	}, investigation.Shown{Evidence: items(1)})

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("an abstention naming nothing must be refused, got %v", err)
	}
}

func TestAdmitOutcome_AnAbstentionNamingAGapIsAdmitted(t *testing.T) {
	t.Parallel()

	gaps := []investigation.Gap{{ID: uuid.New(), Cause: investigation.GapRetentionHorizon}}

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:         investigation.OutcomeAbstained,
		Statement:    "the decisive events had already expired",
		RelevantGaps: []int{1},
	}, investigation.Shown{Evidence: items(1), Gaps: gaps})
	if err != nil {
		t.Fatalf("an abstention naming what was missing must be admitted: %v", err)
	}
	relevant := admitted.Outcome.RelevantGaps
	if len(relevant) != 1 || relevant[0] != gaps[0].ID {
		t.Errorf("the abstention must resolve the gap it named, got %v", relevant)
	}
}

// Every claim must resolve to an evidence item that exists, and the resolution is by identifier
// so that following a citation is a lookup rather than a search.
func TestAdmitOutcome_ClaimsResolveToTheEvidenceTheyCited(t *testing.T) {
	t.Parallel()

	shown := items(3)

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the container cannot reach its configured host",
		Explains:  1,
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "it logs a refused connection",
				Evidence: []int{1, 3}},
			{Role: investigation.ClaimAffectedScope, Statement: "three pods are unready",
				Evidence: []int{2}},
		},
	}, shownWithATestedHypothesis(shown, nil))
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
	outcome := admitted.Outcome
	if len(outcome.Claims) != 2 {
		t.Fatalf("both claims must survive, got %d", len(outcome.Claims))
	}
	cited := outcome.Claims[0].Evidence
	if len(cited) != 2 || cited[0] != shown[0].ID || cited[1] != shown[2].ID {
		t.Errorf("citations must resolve to the items shown, got %v", cited)
	}
	// Affected scope is a cited statement like any other. If it were not, a figure could reach the
	// page with nothing behind it.
	if len(outcome.Claims[1].Evidence) != 1 {
		t.Errorf("an affected-scope statement must carry evidence, got %v", outcome.Claims[1])
	}
}

// One source agreeing with itself is one source. The field is a count of sources, never a score.
func TestAdmitOutcome_IndependentSourcesCountsDistinctConnections(t *testing.T) {
	t.Parallel()

	shared := uuid.New()
	shown := []investigation.Item{
		{ID: uuid.New(), Connection: shared},
		{ID: uuid.New(), Connection: shared},
	}

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload cannot start",
		Explains:  1,
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "twice observed",
				Evidence: []int{1, 2}},
		},
	}, shownWithATestedHypothesis(shown, nil))
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
	if admitted.Outcome.IndependentSources != 1 {
		t.Errorf("two items from one Connection are one source, got %d",
			admitted.Outcome.IndependentSources)
	}
}

// THE TRACED EXPLANATION.
//
// An outcome that states a cause nobody proposed and nobody tested is the failure these assert
// against. It looked identical to a real conclusion in the record, and across three live runs it
// happened twice.

func supporting() []investigation.DraftClaim {
	return []investigation.DraftClaim{
		{Role: investigation.ClaimSupporting, Statement: "the Secret it names is not there",
			Evidence: []int{1}},
	}
}

func hypotheses(states ...investigation.HypothesisState) []investigation.Hypothesis {
	made := make([]investigation.Hypothesis, 0, len(states))
	for index, state := range states {
		made = append(made, investigation.Hypothesis{
			ID:        uuid.New(),
			Ordinal:   index + 1,
			Statement: "an explanation",
			Falsifies: "an observation that would disprove it",
			State:     state,
		})
	}
	return made
}

func TestAdmitOutcome_RefusesASupportedExplanationThatNamesNoHypothesis(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisFalsified, investigation.HypothesisSetAside)

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Claims:    supporting(),
	}, investigation.Shown{Evidence: items(1), Hypotheses: held})

	if !errors.Is(err, investigation.ErrUntraced) {
		t.Fatalf("an explanation traced to no hypothesis must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesAnExplanationTracedToAFalsifiedHypothesis(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisFalsified)

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  1,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[0].ID: {}},
	})

	if !errors.Is(err, investigation.ErrUntraced) {
		t.Fatalf("an explanation traced to a falsified hypothesis must be refused, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "falsified") {
		t.Errorf("the refusal must name the state it found, got %q", err.Error())
	}
}

func TestAdmitOutcome_RefusesAnExplanationTracedToASetAsideHypothesis(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSetAside)

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  1,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[0].ID: {}},
	})

	if !errors.Is(err, investigation.ErrUntraced) {
		t.Fatalf("an explanation the round declined to pursue must be refused, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "set_aside") {
		t.Errorf("the refusal must name the state it found, got %q", err.Error())
	}
}

func TestAdmitOutcome_RefusesAnExplanationTracedToAHypothesisThatWasNeverShown(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSupported)

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start",
		// One hypothesis was shown; this names a second.
		Explains: 2,
		Claims:   supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[0].ID: {}},
	})

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("naming a hypothesis nobody was shown must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesAnAbstentionThatAlsoExplainsAHypothesis(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSupported)

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:       investigation.OutcomeAbstained,
		Statement:  "no explanation is sufficiently supported",
		Explains:   1,
		Unresolved: nil,
		RelevantGaps: []int{
			1,
		},
	}, investigation.Shown{
		Evidence:   items(1),
		Gaps:       []investigation.Gap{{ID: uuid.New()}},
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[0].ID: {}},
	})

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("an abstention that also names an explanation contradicts itself, got %v", err)
	}
}

func TestAdmitOutcome_AnExplanationTracedToATestedSupportedHypothesisIsAdmitted(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisFalsified, investigation.HypothesisSupported)

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  2,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[1].ID: {}},
	})
	if err != nil {
		t.Fatalf("a traced, tested explanation must be admitted: %v", err)
	}
	if admitted.Outcome.Kind != investigation.OutcomeSupported {
		t.Errorf("kind = %s, want supported carried through", admitted.Outcome.Kind)
	}
	if admitted.Outcome.Explains != held[1].ID {
		t.Errorf("the outcome must resolve to the hypothesis it explains, got %v",
			admitted.Outcome.Explains)
	}
	if admitted.Untested {
		t.Error("a hypothesis a dispatched read pointed at was tested")
	}
}

// A hypothesis nothing was read to disprove was never put at risk. The explanation may still
// stand — it is what the evidence says — but calling it supported would make "supported" mean two
// different things, and the difference is the one a reader is buying.
func TestAdmitOutcome_AnExplanationNoReadTestedIsCaveatedRatherThanSupported(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSupported)

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  1,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		// Nothing dispatched pointed at it.
		Tested: nil,
	})
	if err != nil {
		t.Fatalf("an untested explanation is demoted rather than refused: %v", err)
	}
	if admitted.Outcome.Kind != investigation.OutcomeCaveated {
		t.Errorf("kind = %s, want caveated", admitted.Outcome.Kind)
	}
	if !admitted.Untested {
		t.Error("the caller has to be told, because it is the caller that records the gap")
	}
}

// Every model-boundary failure ends the round the same way, so the case file has to say which one
// it was or a reader cannot tell the vendor's problem from this build's defect. The domain declares
// the interface; that is what keeps the taxonomy out of it.
func TestNamedFailure_TheDomainReadsTheFailureNameWithoutKnowingTheTaxonomy(t *testing.T) {
	t.Parallel()

	var boundary investigation.NamedFailure = namedOutage{}
	if boundary.FailureName() != "outage" {
		t.Errorf("FailureName() = %q, want the closed vocabulary word", boundary.FailureName())
	}
	// It must survive wrapping, because the round sees the error after the boundary has wrapped it.
	if !errors.Is(fmt.Errorf("reaching the provider: %w", boundary),
		investigation.ErrModelUnavailable) {
		t.Error("a named failure must still read as the model being unavailable")
	}
}

type namedOutage struct{}

func (namedOutage) Error() string       { return "the model provider is unreachable" }
func (namedOutage) FailureName() string { return "outage" }
func (namedOutage) Unwrap() error       { return investigation.ErrModelUnavailable }

// The two halves of "tested" have to agree on what identifies a hypothesis, and they are written in
// different files: a request carries the identity Admit resolved from an ordinal, and admission
// looks that identity up. If they ever disagreed nothing would fail loudly — every explanation would
// simply be caveated, for a reason no reader could distinguish from the honest one.
func TestAdmit_AJustifiedReadCarriesTheIdentityAdmissionLooksFor(t *testing.T) {
	t.Parallel()

	scope := investigation.Scope{
		Namespace:    "commerce",
		WorkloadName: "orders",
		WorkloadKind: investigation.WorkloadStatefulSet,
	}
	held := hypotheses(investigation.HypothesisLive, investigation.HypothesisLive)

	admission := investigation.Admit(investigation.Proposal{
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments: investigation.Arguments{
			Namespace:    scope.Namespace,
			WorkloadName: scope.WorkloadName,
			WorkloadKind: scope.WorkloadKind,
		},
		Justification: 2,
		Reason:        "the runtime state would disprove it",
	}, investigation.Bounds{
		Scope:      scope,
		Window:     investigation.Window{Start: time.Now().Add(-time.Hour), End: time.Now()},
		Controls:   investigation.DefaultControls(),
		Hypotheses: held,
		Pass:       1,
	})

	if !admission.Admitted {
		t.Fatalf("the read must be admitted, refused as %s", admission.Refusal)
	}
	if admission.Request.Justification != held[1].ID {
		t.Errorf("the request carries %v; admission looks up %v",
			admission.Request.Justification, held[1].ID)
	}
}

// The reasoner's own kind is never promoted. It is demoted or left alone, because a model asked to
// grade its own rigour grades it generously.
func TestAdmitOutcome_ACaveatedDraftIsNotPromotedByBeingTested(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSupported)

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeCaveated,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  1,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     map[uuid.UUID]struct{}{held[0].ID: {}},
	})
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
	if admitted.Outcome.Kind != investigation.OutcomeCaveated {
		t.Errorf("kind = %s, want caveated left alone", admitted.Outcome.Kind)
	}
	if admitted.Untested {
		t.Error("a tested hypothesis is not untested merely because the draft was caveated")
	}
}

// A caveated draft resting on an untested hypothesis is still untested. The kind does not move —
// there is nowhere below caveated to move it to — but the gap must still be recorded, or a reader
// sees a caveat and no reason for it.
func TestAdmitOutcome_ACaveatedDraftOnAnUntestedHypothesisStillReportsUntested(t *testing.T) {
	t.Parallel()

	held := hypotheses(investigation.HypothesisSupported)

	admitted, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeCaveated,
		Statement: "the pod cannot start because a Secret it references does not exist",
		Explains:  1,
		Claims:    supporting(),
	}, investigation.Shown{
		Evidence:   items(1),
		Hypotheses: held,
		Tested:     nil,
	})
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
	if admitted.Outcome.Kind != investigation.OutcomeCaveated {
		t.Errorf("kind = %s, want caveated", admitted.Outcome.Kind)
	}
	if !admitted.Untested {
		t.Error("the gap has to be recorded whatever kind the reasoner drafted")
	}
}

// Absence is only a fact with a completeness certificate. A truncated read has none, and the
// convenient version of this — admit it and note the doubt — is how an RBAC misconfiguration
// becomes a certified negative.
func TestValidateAbsence_RefusesAReadWithNoCertificate(t *testing.T) {
	t.Parallel()

	window := investigation.Window{
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
	}
	candidate := investigation.Candidate{
		Observation:  investigation.Observation{Statement: "no events were recorded"},
		CapabilityID: "kubernetes.namespace.events",
		Connection:   uuid.New(),
		Trust:        investigation.TrustRelayAttested,
		Certificate:  &investigation.Certificate{PaginationComplete: false, FullyAuthorized: true},
	}

	if _, err := investigation.ValidateAbsence(candidate, window); !errors.Is(err, investigation.ErrEvidence) {
		t.Fatalf("a truncated read must not support an absence claim, got %v", err)
	}

	candidate.Certificate.PaginationComplete = true
	candidate.Certificate.FullyAuthorized = false
	if _, err := investigation.ValidateAbsence(candidate, window); !errors.Is(err, investigation.ErrEvidence) {
		t.Fatalf("a partially authorised read must not support an absence claim, got %v", err)
	}

	candidate.Certificate.FullyAuthorized = true
	admitted, err := investigation.ValidateAbsence(candidate, window)
	if err != nil {
		t.Fatalf("a complete, fully authorised read must support an absence claim: %v", err)
	}
	if !admitted.Absence {
		t.Error("the admitted item must be marked as an absence")
	}
	// The trust class travels with it and is not promoted: an attestation is not a verification.
	if admitted.Trust != investigation.TrustRelayAttested {
		t.Errorf("trust class = %v, want relay_attested carried through unpromoted", admitted.Trust)
	}
}

// An item whose source time falls outside the window is kept but not placed on the timeline:
// inventing an ordering for it would move the ordering the timeline is read for.
func TestValidate_AnObservationOutsideTheWindowIsNotOnTheTimeline(t *testing.T) {
	t.Parallel()

	window := investigation.Window{
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
	}
	item, err := investigation.Validate(investigation.Candidate{
		Observation: investigation.Observation{
			Statement:        "a pod was evicted",
			SourceObservedAt: window.Start.Add(-24 * time.Hour),
		},
		CapabilityID: "kubernetes.namespace.events",
		Connection:   uuid.New(),
		Trust:        investigation.TrustRelayAttested,
	}, window)
	if err != nil {
		t.Fatalf("validating: %v", err)
	}
	if item.OnTimeline() {
		t.Error("an observation from outside the window must be listed beside the timeline")
	}
}

// Controls compose by most-restrictive, uniformly. A control nobody configured must not become
// the strictest one in the composition.
func TestControls_ComposeByMostRestrictive(t *testing.T) {
	t.Parallel()

	product := investigation.Controls{
		MaxRequests:           8,
		MaxResultBytes:        2 << 20,
		Deadline:              5 * time.Minute,
		PermittedCapabilities: []string{"a", "b", "c"},
	}
	customer := investigation.Controls{
		MaxRequests: 3,
		// MaxResultBytes and Deadline are unset: this side restricts nothing there.
		PermittedCapabilities: []string{"b", "c", "d"},
	}

	composed := product.Restrict(customer)

	if composed.MaxRequests != 3 {
		t.Errorf("MaxRequests = %d, want the smaller of the two", composed.MaxRequests)
	}
	if composed.MaxResultBytes != 2<<20 {
		t.Errorf("MaxResultBytes = %d; an unset restriction must not win", composed.MaxResultBytes)
	}
	if composed.Deadline != 5*time.Minute {
		t.Errorf("Deadline = %s; an unset restriction must not win", composed.Deadline)
	}
	if len(composed.PermittedCapabilities) != 2 {
		t.Errorf("permitted capabilities = %v, want the intersection",
			composed.PermittedCapabilities)
	}
	if composed.Permits("a") || !composed.Permits("b") {
		t.Errorf("the intersection must exclude a and keep b, got %v",
			composed.PermittedCapabilities)
	}
}

// A capability the customer's stack does not provide is not a gap and must not be reported as
// one.
func TestCoverageState_NotApplicableIsNotAGap(t *testing.T) {
	t.Parallel()

	for state, isGap := range map[investigation.CoverageState]bool{
		investigation.CoverageChecked:       false,
		investigation.CoverageCheckedEmpty:  false,
		investigation.CoverageIncomplete:    true,
		investigation.CoverageUnavailable:   true,
		investigation.CoverageNotApplicable: false,
	} {
		if state.IsGap() != isGap {
			t.Errorf("%s.IsGap() = %v, want %v", state, state.IsGap(), isGap)
		}
	}
}

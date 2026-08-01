package investigation_test

import (
	"errors"
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

func TestAdmitOutcome_RefusesAnUncitedClaim(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the container exits because its configuration names an unreachable host",
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "it exits with code 1"},
		},
	}, items(2), nil, nil)

	if !errors.Is(err, investigation.ErrUncited) {
		t.Fatalf("a claim citing nothing must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesACitationOfEvidenceThatWasNeverShown(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload is failing",
		Claims: []investigation.DraftClaim{
			// Two items were shown; this cites a third.
			{Role: investigation.ClaimSupporting, Statement: "it exits", Evidence: []int{3}},
		},
	}, items(2), nil, nil)

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("citing evidence that was never shown must be refused, got %v", err)
	}
}

func TestAdmitOutcome_RefusesASupportedExplanationWithNoSupportingClaim(t *testing.T) {
	t.Parallel()

	_, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload is failing",
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimContradicting, Statement: "the node is healthy",
				Evidence: []int{1}},
		},
	}, items(2), nil, nil)

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
	}, items(1), nil, nil)

	if !errors.Is(err, investigation.ErrOutcome) {
		t.Fatalf("an abstention naming nothing must be refused, got %v", err)
	}
}

func TestAdmitOutcome_AnAbstentionNamingAGapIsAdmitted(t *testing.T) {
	t.Parallel()

	gaps := []investigation.Gap{{ID: uuid.New(), Cause: investigation.GapRetentionHorizon}}

	outcome, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:         investigation.OutcomeAbstained,
		Statement:    "the decisive events had already expired",
		RelevantGaps: []int{1},
	}, items(1), gaps, nil)
	if err != nil {
		t.Fatalf("an abstention naming what was missing must be admitted: %v", err)
	}
	if len(outcome.RelevantGaps) != 1 || outcome.RelevantGaps[0] != gaps[0].ID {
		t.Errorf("the abstention must resolve the gap it named, got %v", outcome.RelevantGaps)
	}
}

// Every claim must resolve to an evidence item that exists, and the resolution is by identifier
// so that following a citation is a lookup rather than a search.
func TestAdmitOutcome_ClaimsResolveToTheEvidenceTheyCited(t *testing.T) {
	t.Parallel()

	shown := items(3)

	outcome, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the container cannot reach its configured host",
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "it logs a refused connection",
				Evidence: []int{1, 3}},
			{Role: investigation.ClaimAffectedScope, Statement: "three pods are unready",
				Evidence: []int{2}},
		},
	}, shown, nil, nil)
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
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

	outcome, err := investigation.AdmitOutcome(investigation.Draft{
		Kind:      investigation.OutcomeSupported,
		Statement: "the workload cannot start",
		Claims: []investigation.DraftClaim{
			{Role: investigation.ClaimSupporting, Statement: "twice observed",
				Evidence: []int{1, 2}},
		},
	}, shown, nil, nil)
	if err != nil {
		t.Fatalf("admitting: %v", err)
	}
	if outcome.IndependentSources != 1 {
		t.Errorf("two items from one Connection are one source, got %d",
			outcome.IndependentSources)
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

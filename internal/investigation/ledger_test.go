package investigation

import (
	"strings"
	"testing"
	"time"
)

// The change ledger is a navigation index and never evidence. That rule is enforced by
// construction — a ledger entry carries no capability read, no connection read and no
// trust class, and EvidenceValidation refuses all three — and these tests state the
// refusal so it cannot regress into review's job.

func TestValidate_ALedgerChangeIsRefusedAsEvidence(t *testing.T) {
	window := Window{
		Start: time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC),
	}
	// The closest a ledger entry could come to a candidate: a statement with a time, and
	// nothing behind it — no capability produced it, no connection was read to make it,
	// no trust class attaches to it.
	fromLedger := Candidate{
		Observation: Observation{
			Statement:        "deployment api: spec.replicas 3 -> 5",
			SourceObservedAt: window.Start.Add(30 * time.Minute),
			ReceivedAt:       window.Start.Add(31 * time.Minute),
		},
	}
	if _, err := Validate(fromLedger, window); err == nil {
		t.Fatal("a ledger change must be refused as an EvidenceItem; a conclusion resting " +
			"on one revalidates live and cites what the revalidation produced")
	}
}

func TestLedgerGaps_NameTheBoundaryAndItsConsequence(t *testing.T) {
	window := Window{
		Start: time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC),
	}
	covered := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)

	horizon := LedgerHorizonGap(covered, window)
	if horizon.Cause != GapLedgerHorizon || horizon.Consequence == "" {
		t.Fatalf("a gap with no consequence is a defect, got %+v", horizon)
	}
	if !strings.Contains(horizon.Consequence, "2026-08-05T03:00:00Z") {
		t.Fatalf("the boundary must be named so silence before it is not read as calm: %q",
			horizon.Consequence)
	}

	stale := LedgerStaleGap(covered, false, false)
	if stale.Cause != GapLedgerStale || !strings.Contains(stale.Consequence, "2026-08-05T03:00:00Z") {
		t.Fatalf("the last confirmed instant must be named, got %+v", stale)
	}
	faulted := LedgerStaleGap(covered, true, false)
	if !strings.Contains(faulted.Consequence, "could not read the cluster") {
		t.Fatalf("a faulted scope must say why it is stale, got %q", faulted.Consequence)
	}
}

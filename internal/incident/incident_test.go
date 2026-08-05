package incident_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/incident"
)

// The vocabulary, asserted where nothing else can reach it.
//
// Grouping itself is asserted through the intake listener and the operator API at the composition
// root, because that is where an operator observes it. What is left here is the small set of
// properties that have no observable surface until they are already wrong: a status nobody can
// name, a basis nobody can explain, a merge admitted that means nothing.

// Every persisted status renders as something a caller can send back. A value that rendered as
// "unrecognised" would appear in a listing and then be refused as a filter, which is a surface
// that contradicts itself.
func TestEveryStatusRoundTripsThroughTheNameItIsShownUnder(t *testing.T) {
	t.Parallel()

	for _, status := range []incident.Status{incident.StatusOpen, incident.StatusResolved} {
		name := status.String()
		if name == "unrecognised" {
			t.Errorf("status %d renders as unrecognised", status)
			continue
		}
		parsed, known := incident.ParseStatus(name)
		if !known || parsed != status {
			t.Errorf("%q parses back to %v (known=%t), want %v", name, parsed, known, status)
		}
	}
}

// A status nobody typed is REFUSED rather than resolved to something. A filter silently narrowed
// to a value nobody asked for answers a different question, and an empty page is exactly what "you
// have none of those" looks like.
func TestAStatusNobodyTypedIsRefusedRatherThanGuessedAt(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "Open", "resolvd", "closed", "OPEN", " open"} {
		if _, known := incident.ParseStatus(value); known {
			t.Errorf("%q was accepted as a status", value)
		}
	}
}

// Every basis says who decided the grouping, in words. A basis added later without an explanation
// would render as a badge an operator cannot act on, which defeats the reason the field exists.
func TestEveryGroupingBasisCanBeExplainedToAnOperator(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, basis := range []incident.Basis{
		incident.BasisSourceGrouping, incident.BasisUngrouped,
	} {
		if basis.String() == "unrecognised" {
			t.Errorf("basis %d renders as unrecognised", basis)
		}
		explanation := basis.Explain()
		if explanation == "" {
			t.Errorf("basis %s explains nothing", basis)
		}
		if seen[explanation] {
			t.Errorf("basis %s repeats another basis's explanation; two groupings that read the "+
				"same are two an operator cannot tell apart", basis)
		}
		seen[explanation] = true
	}
}

// An unrecorded value is inert rather than an error nobody handles, which is what makes a row
// written by a newer build safe to read.
func TestAnUnrecordedValueIsInertRatherThanMistakenForADeclaredOne(t *testing.T) {
	t.Parallel()

	if incident.Status(99).String() != "unrecognised" {
		t.Error("an undeclared status claims to be one of ours")
	}
	if incident.Basis(99).String() != "unrecognised" {
		t.Error("an undeclared basis claims to be one of ours")
	}
	if incident.Basis(99).Explain() == incident.BasisSourceGrouping.Explain() {
		t.Error("an undeclared basis explains itself as a source grouping, which would tell an " +
			"operator their alerting made a decision it did not make")
	}
}

// A merge that could not mean anything is refused before any row is read, and the refusal names
// which reason applies — the caller is an operator correcting a grouping, and one they cannot act
// on is a defect.
func TestAMergeThatCouldNotMeanAnythingIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()

	one, two := uuid.New(), uuid.New()
	for name, merge := range map[string]incident.Merge{
		"neither named":  {Reason: "because"},
		"absorbed only":  {Absorbed: one, Reason: "because"},
		"survivor only":  {Into: two, Reason: "because"},
		"into itself":    {Absorbed: one, Into: one, Reason: "because"},
		"no reason":      {Absorbed: one, Into: two},
		"endless reason": {Absorbed: one, Into: two, Reason: overlong()},
	} {
		if err := merge.Validate(); err == nil {
			t.Errorf("a merge with %s was admitted", name)
		}
	}

	whole := incident.Merge{Absorbed: one, Into: two, Reason: "one rollout, two alerts"}
	if err := whole.Validate(); err != nil {
		t.Errorf("a merge naming two episodes and a reason was refused: %v", err)
	}
}

func overlong() string {
	runes := make([]rune, incident.MaxReasonLength+1)
	for index := range runes {
		runes[index] = 'a'
	}
	return string(runes)
}

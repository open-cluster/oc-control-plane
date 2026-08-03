package reasoning_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Well-formed answers, one per method, and what the boundary makes of them.

const goodHypotheses = `{"hypotheses":[
  {"statement":"the new image crashes on startup","falsifies":"a pod on the new image stays ready"},
  {"statement":"the node is evicting the pod","falsifies":"no eviction event names this pod"}
]}`

const goodProposals = `{
  "proposals":[{"capability":"kubernetes.container.logs","justification":1,
    "reason":"the previous instance's log would say why it exited",
    "arguments":{"pod":2,"container":"checkout","previous":true,
      "max_pods":0,"max_events":0,"max_lines":200}}],
  "weighings":[{"hypothesis":1,"evidence":1,"stance":"supports",
    "reason":"a backoff loop is what a startup crash looks like from outside"}],
  "settlings":[]}`

const goodConclusion = `{
  "kind":"supported",
  "statement":"the deployment's new image exits on startup and the pod backs off",
  "claims":[{"role":"supporting","statement":"the container is in a backoff loop","evidence":[1]}],
  "unresolved":[],
  "relevant_gaps":[1],
  "weighings":[],
  "settlings":[{"hypothesis":2,"state":"falsified","reason":"no eviction event names this pod"}]}`

func TestHypotheses_BecomeTheTypedValueTheBoundaryReturns(t *testing.T) {
	provider := newFakeProvider("primary", answer{
		document: goodHypotheses,
		usage:    usageOf(1000, 200, 0, 0),
	})
	service := serviceUnder(t, provider)

	proposed, err := service.Hypotheses(context.Background(), briefFixture())
	if err != nil {
		t.Fatalf("proposing hypotheses: %v", err)
	}

	if len(proposed.Hypotheses) != 2 {
		t.Fatalf("got %d hypotheses, want 2", len(proposed.Hypotheses))
	}
	first := proposed.Hypotheses[0]
	if first.Ordinal != 1 {
		t.Errorf("the first hypothesis is ordinal %d, want 1", first.Ordinal)
	}
	if first.State != investigation.HypothesisLive {
		t.Errorf("a proposed hypothesis is %s, want live", first.State)
	}
	if first.Falsifies == "" {
		t.Error("a hypothesis carries no falsification condition")
	}
	// 1000 input tokens at $1 per million plus 200 output tokens at $10 per million, in
	// micro-cents and rounded half-up. The figure has to come from both rates: costing a round
	// from one of them is what reports the cheapest rounds as the most expensive.
	wantCost := int64((1000*100_000_000 + 200*1_000_000_000 + 500_000) / 1_000_000)
	if proposed.Usage.MicroCents != wantCost {
		t.Errorf("the round was costed at %d micro-cents, want %d",
			proposed.Usage.MicroCents, wantCost)
	}
	if proposed.Usage.Tokens != 1200 {
		t.Errorf("the round consumed %d tokens, want 1200", proposed.Usage.Tokens)
	}
}

func TestRequests_CarryProposalsWeighingsAndSettlingsIntact(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: goodProposals})
	service := serviceUnder(t, provider)

	proposed, err := service.Requests(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("proposing reads: %v", err)
	}

	if len(proposed.Proposals) != 1 {
		t.Fatalf("got %d proposals, want 1", len(proposed.Proposals))
	}
	read := proposed.Proposals[0]
	if read.Justification != 1 {
		t.Errorf("the read points at hypothesis %d, want 1", read.Justification)
	}
	// The pod was named by ORDINAL and resolved here against what the brief actually found. This
	// is the invariant that stops a proposed read naming a pod nobody resolved.
	if read.Arguments.PodName != "checkout-7d9f-bbbbb" {
		t.Errorf("the read names pod %q, want the second pod the brief resolved",
			read.Arguments.PodName)
	}
	if read.Arguments.Namespace != "payments" || read.Arguments.WorkloadName != "checkout" {
		t.Errorf("the read was not filled in from the case's own scope: %+v", read.Arguments)
	}
	if !read.Arguments.Previous {
		t.Error("the read did not ask for the previous container instance")
	}
	if len(proposed.Weighings) != 1 || proposed.Weighings[0].Stance != investigation.StanceSupports {
		t.Errorf("the weighing did not survive decoding: %+v", proposed.Weighings)
	}
}

func TestConclude_CarriesTheDraftAndItsSettlings(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: goodConclusion})
	service := serviceUnder(t, provider)

	concluded, err := service.Conclude(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("concluding: %v", err)
	}

	if concluded.Draft.Kind != investigation.OutcomeSupported {
		t.Errorf("the draft is kind %s, want supported", concluded.Draft.Kind)
	}
	if len(concluded.Draft.Claims) != 1 || len(concluded.Draft.Claims[0].Evidence) != 1 {
		t.Fatalf("the claim did not survive decoding: %+v", concluded.Draft.Claims)
	}
	if len(concluded.Settlings) != 1 {
		t.Errorf("got %d settlings, want 1", len(concluded.Settlings))
	}
}

// One rule for ordinals at every call: they name what the reasoner was shown plus what the same
// answer proposed, and nothing translates them on the way to the domain. An earlier build admitted
// the conclusion against the hypotheses still LIVE after its own settlings, so this package had to
// renumber — and a renumbering that drifted from the domain's list would have refused sound
// conclusions for naming a hypothesis that exists.
func TestConclude_UnresolvedOrdinalsNameTheHypothesesTheReasonerWasShown(t *testing.T) {
	// The reasoner settles hypothesis 1 and calls hypothesis 2 unresolved. Both ordinals name the
	// list it answered in, which is the list the domain now checks them against — the round holds
	// every hypothesis it proposed, settled or not.
	document := `{
	  "kind":"abstained","statement":"nothing is sufficiently supported","explains":0,
	  "hypotheses":[],"claims":[],"unresolved":[2],"relevant_gaps":[1],"weighings":[],
	  "settlings":[{"hypothesis":1,"state":"falsified","reason":"a pod on the new image is ready"}]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	concluded, err := service.Conclude(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("concluding: %v", err)
	}
	if len(concluded.Draft.Unresolved) != 1 || concluded.Draft.Unresolved[0] != 2 {
		t.Fatalf("unresolved is %v, want [2] — the ordinal the reasoner answered in",
			concluded.Draft.Unresolved)
	}

	// The proof that the ordinals mean what this package says they mean: the domain admits them.
	held := deliberationFixture().Hypotheses
	held[0].State = investigation.HypothesisFalsified
	if _, admitErr := investigation.AdmitOutcome(concluded.Draft, investigation.Shown{
		Evidence:   deliberationFixture().Evidence,
		Gaps:       deliberationFixture().Gaps,
		Hypotheses: held,
	}); admitErr != nil {
		t.Errorf("the domain refused a draft this package produced: %v", admitErr)
	}
}

// THE TRACED EXPLANATION, AT THE DECODING SEAM.
//
// The domain refuses an explanation that names no supported hypothesis. These assert the half this
// package owns: that a reasoner CAN name one it discovered, and that it cannot name one that will
// not exist.

func TestConclude_AConclusionMayExplainAHypothesisItProposedInTheSameDocument(t *testing.T) {
	// Two were shown; this proposes a third and explains it. Without that the only way to state a
	// cause the evidence revealed would be to attach it to nothing.
	document := `{
	  "kind":"supported","statement":"the Secret it references does not exist","explains":3,
	  "hypotheses":[{"statement":"a referenced Secret is absent",
	                 "falsifies":"the Secret exists in the namespace"}],
	  "claims":[{"role":"supporting","statement":"the container never started","evidence":[1]}],
	  "unresolved":[],"relevant_gaps":[],"weighings":[],
	  "settlings":[{"hypothesis":3,"state":"supported","reason":"the event names the Secret"}]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	concluded, err := service.Conclude(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("concluding: %v", err)
	}
	if len(concluded.Hypotheses) != 1 {
		t.Fatalf("the proposed hypothesis must survive decoding, got %d",
			len(concluded.Hypotheses))
	}
	if concluded.Hypotheses[0].Ordinal != 3 {
		t.Errorf("ordinal = %d, want the next after the two it was shown",
			concluded.Hypotheses[0].Ordinal)
	}
	if concluded.Draft.Explains != 3 {
		t.Errorf("explains = %d, want the hypothesis it proposed", concluded.Draft.Explains)
	}
}

// The prompt invites the planner to discover a hypothesis and then ask for a read that would test
// it. Bounding the justification by what it was SHOWN would refuse the whole document for following
// the instruction it was given, and the failure would read as the model being malformed.
func TestRequests_AReadMayPointAtAHypothesisTheSameAnswerProposed(t *testing.T) {
	// Two were shown; this proposes a third and points a read at it.
	document := `{
	  "proposals":[{"capability":"kubernetes.container.logs","justification":3,
	                "reason":"what it said before it died would disprove the new explanation",
	                "arguments":{"pod":2,"container":"checkout","previous":true,
	                             "max_pods":0,"max_events":0,"max_lines":0}}],
	  "hypotheses":[{"statement":"a referenced Secret is absent",
	                 "falsifies":"the Secret exists in the namespace"}],
	  "weighings":[{"hypothesis":3,"evidence":1,"stance":"supports",
	                "reason":"the event names a missing Secret"}],
	  "settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	proposed, err := service.Requests(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("proposing: %v", err)
	}
	if len(proposed.Proposals) != 1 || proposed.Proposals[0].Justification != 3 {
		t.Fatalf("the read must survive pointing at the hypothesis just proposed, got %+v",
			proposed.Proposals)
	}
	if len(proposed.Hypotheses) != 1 || proposed.Hypotheses[0].Ordinal != 3 {
		t.Errorf("the hypothesis must take the next ordinal, got %+v", proposed.Hypotheses)
	}
	// Weighed against evidence it was shown: the discovery came FROM that evidence, and recording
	// how it stands is most of what shows the discovery was reasoning rather than assertion.
	if len(proposed.Weighings) != 1 || proposed.Weighings[0].Hypothesis != 3 {
		t.Errorf("the weighing must survive naming the hypothesis just proposed, got %+v",
			proposed.Weighings)
	}
}

// A reasoner shown a `hypotheses` field fills it with the whole list it is holding rather than only
// what is new. It did exactly that on a live run, leaving a case with seven hypotheses of which
// three were word-for-word repeats — the same explanation twice in the record, twice in the
// supported state. Refusing the document would cost a round; a restatement is dropped and every
// ordinal pointing at it is redirected to the hypothesis it restates, because that is what the
// reasoner meant.
func TestRequests_AHypothesisRestatingOneAlreadyHeldIsDroppedNotDuplicated(t *testing.T) {
	// Hypothesis 1 of the fixture, word for word, plus one genuinely new explanation. The read and
	// the settling both point at the restatement's ordinal, 3.
	document := `{
	  "proposals":[{"capability":"kubernetes.namespace.events","justification":3,
	                "reason":"the cluster's own account would disprove the restated explanation",
	                "arguments":{"pod":0,"container":"","previous":false,
	                             "max_pods":0,"max_events":0,"max_lines":0}}],
	  "hypotheses":[{"statement":"the new image crashes on startup",
	                 "falsifies":"a pod running the new image stays ready"},
	                {"statement":"a referenced Secret is absent",
	                 "falsifies":"the Secret exists in the namespace"}],
	  "weighings":[{"hypothesis":3,"evidence":1,"stance":"supports","reason":"it backs the restated one"}],
	  "settlings":[{"hypothesis":4,"state":"supported","reason":"the event names a missing Secret"}]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	proposed, err := service.Requests(context.Background(), deliberationFixture())
	if err != nil {
		t.Fatalf("proposing: %v", err)
	}
	if len(proposed.Hypotheses) != 1 {
		t.Fatalf("only the genuinely new explanation may be appended, got %d: %+v",
			len(proposed.Hypotheses), proposed.Hypotheses)
	}
	if proposed.Hypotheses[0].Statement != "a referenced Secret is absent" {
		t.Errorf("the wrong one survived: %q", proposed.Hypotheses[0].Statement)
	}
	// Two were shown, one is appended, so the new one is ordinal 3 rather than the 4 the reasoner
	// used — and everything the reasoner pointed at those ordinals moves with them.
	if proposed.Hypotheses[0].Ordinal != 3 {
		t.Errorf("ordinal = %d, want 3", proposed.Hypotheses[0].Ordinal)
	}
	if len(proposed.Proposals) != 1 || proposed.Proposals[0].Justification != 1 {
		t.Errorf("the read pointed at the restatement of hypothesis 1 and must resolve to 1, got %+v",
			proposed.Proposals)
	}
	if len(proposed.Weighings) != 1 || proposed.Weighings[0].Hypothesis != 1 {
		t.Errorf("the weighing must resolve to 1, got %+v", proposed.Weighings)
	}
	if len(proposed.Settlings) != 1 || proposed.Settlings[0].Hypothesis != 3 {
		t.Errorf("the settling named the new explanation and must resolve to 3, got %+v",
			proposed.Settlings)
	}
}

func TestRequests_AReadPointingPastEverythingShownAndProposedIsRefused(t *testing.T) {
	document := `{
	  "proposals":[{"capability":"kubernetes.namespace.events","justification":4,
	                "reason":"it would disprove something",
	                "arguments":{"pod":0,"container":"","previous":false,
	                             "max_pods":0,"max_events":0,"max_lines":0}}],
	  "hypotheses":[],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	if _, err := service.Requests(
		context.Background(), deliberationFixture()); !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a malformed-output failure", err)
	}
}

func TestConclude_AProposedHypothesisWithNothingThatWouldDisproveItIsRefused(t *testing.T) {
	// An explanation nothing could disprove is a belief, and letting one in through the late-
	// proposal path would make the falsification condition optional wherever it matters most.
	document := `{
	  "kind":"supported","statement":"the Secret it references does not exist","explains":3,
	  "hypotheses":[{"statement":"a referenced Secret is absent","falsifies":""}],
	  "claims":[{"role":"supporting","statement":"the container never started","evidence":[1]}],
	  "unresolved":[],"relevant_gaps":[],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	if _, err := service.Conclude(
		context.Background(), deliberationFixture()); !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a malformed-output failure", err)
	}
}

func TestConclude_ExplainingAHypothesisThatWillNotExistIsRefused(t *testing.T) {
	// Two shown, none proposed: ordinal 3 names a hypothesis that will never be in the round.
	document := `{
	  "kind":"supported","statement":"something","explains":3,"hypotheses":[],
	  "claims":[{"role":"supporting","statement":"y","evidence":[1]}],
	  "unresolved":[],"relevant_gaps":[],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	if _, err := service.Conclude(
		context.Background(), deliberationFixture()); !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a malformed-output failure", err)
	}
}

// Refusing what could not be checked.

func TestAnswer_AnOrdinalOutsideWhatWasShownIsRefused(t *testing.T) {
	cases := map[string]string{
		"a claim citing evidence nobody showed": `{"kind":"supported","statement":"x",
			"claims":[{"role":"supporting","statement":"y","evidence":[99]}],
			"unresolved":[],"relevant_gaps":[],"weighings":[],"settlings":[]}`,
		"a coverage gap nobody showed": `{"kind":"abstained","statement":"x","claims":[],
			"unresolved":[],"relevant_gaps":[99],"weighings":[],"settlings":[]}`,
		"a hypothesis nobody showed": `{"kind":"abstained","statement":"x","claims":[],
			"unresolved":[99],"relevant_gaps":[1],"weighings":[],"settlings":[]}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			provider := newFakeProvider("primary", answer{document: document})
			service := serviceUnder(t, provider)

			_, err := service.Conclude(context.Background(), deliberationFixture())
			if !errors.Is(err, reasoning.ErrMalformed) {
				t.Fatalf("got %v, want a malformed-output failure", err)
			}
		})
	}
}

func TestAnswer_AClaimThatCitesNothingIsRefused(t *testing.T) {
	document := `{"kind":"supported","statement":"the image crashes",
		"claims":[{"role":"supporting","statement":"it crashes","evidence":[]}],
		"unresolved":[],"relevant_gaps":[],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	_, err := service.Conclude(context.Background(), deliberationFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want an uncited claim to be refused", err)
	}
	if !strings.Contains(err.Error(), "cites no evidence") {
		t.Errorf("the refusal does not say what was wrong: %v", err)
	}
}

func TestRequests_APodOrdinalNobodyResolvedIsRefused(t *testing.T) {
	document := `{"proposals":[{"capability":"kubernetes.container.logs","justification":1,
		"reason":"read the log","arguments":{"pod":9,"container":"checkout","previous":false,
		"max_pods":0,"max_events":0,"max_lines":0}}],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	_, err := service.Requests(context.Background(), deliberationFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a pod outside what the brief resolved to be refused", err)
	}
}

func TestRequests_AReadThatPointsAtNoHypothesisIsRefused(t *testing.T) {
	document := `{"proposals":[{"capability":"kubernetes.namespace.events","justification":0,
		"reason":"look around","arguments":{"pod":0,"container":"","previous":false,
		"max_pods":0,"max_events":0,"max_lines":0}}],"weighings":[],"settlings":[]}`
	provider := newFakeProvider("primary", answer{document: document})
	service := serviceUnder(t, provider)

	_, err := service.Requests(context.Background(), deliberationFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want an unjustified read to be refused", err)
	}
}

// The bounded retry.

func TestAnswer_AMalformedAnswerIsRetriedExactlyOnce(t *testing.T) {
	provider := newFakeProvider("primary",
		answer{document: `{"hypotheses":[]}`, usage: usageOf(100, 10, 0, 0)},
		answer{document: goodHypotheses, usage: usageOf(100, 10, 0, 0)})
	service := serviceUnder(t, provider)

	proposed, err := service.Hypotheses(context.Background(), briefFixture())
	if err != nil {
		t.Fatalf("the retry did not recover the round: %v", err)
	}
	if len(proposed.Hypotheses) != 2 {
		t.Errorf("got %d hypotheses after the retry, want 2", len(proposed.Hypotheses))
	}
	if provider.callCount() != 2 {
		t.Errorf("the provider was asked %d times, want exactly 2", provider.callCount())
	}
	// Both attempts cost money, and a round that hid the cost of its retries is one nobody could
	// price.
	if proposed.Usage.Tokens != 220 {
		t.Errorf("the retry's tokens were not counted: got %d, want 220", proposed.Usage.Tokens)
	}
}

func TestAnswer_APersistentlyMalformedAnswerFailsTheRoundWithABoundedRetry(t *testing.T) {
	provider := newFakeProvider("primary", answer{document: `{"hypotheses":[]}`})
	service := serviceUnder(t, provider)

	_, err := service.Hypotheses(context.Background(), briefFixture())
	if !errors.Is(err, reasoning.ErrMalformed) {
		t.Fatalf("got %v, want a malformed-output failure", err)
	}
	if provider.callCount() != 2 {
		t.Errorf("the provider was asked %d times, want exactly 2 — an unbounded retry loop "+
			"spends a budget quietly", provider.callCount())
	}
	// Every failure here still ends the round honestly through the domain's own error.
	if !errors.Is(err, investigation.ErrModelUnavailable) {
		t.Error("a failed reasoning step did not read as the model being unavailable to the round")
	}
}

// Every outcome is distinguishable, and none of them is an abstention.

func TestFailures_AreNamedDistinctlyAndNoneIsAnAbstention(t *testing.T) {
	outcomes := []reasoning.Outcome{
		reasoning.OutcomeRefused,
		reasoning.OutcomeOutage,
		reasoning.OutcomeRejected,
		reasoning.OutcomeMalformed,
		reasoning.OutcomeTimeout,
		reasoning.OutcomeCeilingReached,
	}
	seen := make(map[string]reasoning.Outcome, len(outcomes))
	for _, outcome := range outcomes {
		name := outcome.String()
		if existing, duplicate := seen[name]; duplicate {
			t.Fatalf("%v and %v both render as %q", existing, outcome, name)
		}
		seen[name] = outcome

		failure := reasoning.Failed(outcome, "primary", "model-a", "detail")
		named, ok := reasoning.OutcomeOf(failure)
		if !ok || named != outcome {
			t.Errorf("%s did not survive being wrapped in a failure", name)
		}
		// The one that matters: a failure ends the round, and a round that ended this way is
		// FAILED rather than abstained. An abstention is a finding about the evidence.
		if !errors.Is(failure, investigation.ErrModelUnavailable) {
			t.Errorf("%s does not end the round through the domain's own error", name)
		}
	}
}

func TestFailures_ARefusalIsToldApartFromEveryOtherOutcome(t *testing.T) {
	refusal := reasoning.Failed(reasoning.OutcomeRefused, "primary", "model-a", "declined")
	if !errors.Is(refusal, reasoning.ErrRefused) {
		t.Error("a refusal does not read as a refusal")
	}
	for _, other := range []error{
		reasoning.ErrOutage, reasoning.ErrRejected, reasoning.ErrMalformed,
		reasoning.ErrTimeout, reasoning.ErrCeilingReached,
	} {
		if errors.Is(refusal, other) {
			t.Errorf("a refusal also reads as %v, which would let the two be confused", other)
		}
	}
}

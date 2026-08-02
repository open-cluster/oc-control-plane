package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// What the run produced, in the form somebody has to score.
//
// Everything printed here is a fact the run observed. There is deliberately no verdict: whether the
// explanation is any good is a human judgement, and a program that scored its own output would be
// producing the number with the least information in it.

type report struct {
	deployment reasoning.Deployment
	support    reasoning.Support
	started    time.Time
	finished   time.Time

	hypotheses   []investigation.Hypothesis
	proposals    []investigation.Proposal
	served       []investigation.Item
	refusedReads []string
	weighings    []investigation.Weighing
	settlings    []investigation.Settling
	draft        investigation.Draft
	outcome      *investigation.Outcome

	// admissionFailure is the output schema refusing the draft. It is a CONTRACT failure and is
	// reported as one rather than folded into the outcome.
	admissionFailure error
	// failed is the run not completing at all.
	failed error

	records []reasoning.Record
}

func (r *report) print() {
	r.section("THE RUN")
	fmt.Printf("Scenario:          red herring (a deploy 30 minutes before a credential rotation)\n")
	fmt.Printf("Provider/model:    %s/%s, effort %s\n",
		r.deployment.Provider, r.deployment.Model, r.deployment.Effort)
	fmt.Printf("Capabilities:      %s\n", r.support.Describe())
	fmt.Printf("Prompt/schema:     prompt %s, schema %s\n",
		reasoning.PromptVersion, reasoning.SchemaVersion)
	fmt.Printf("Wall clock:        %s\n", r.finished.Sub(r.started).Round(time.Millisecond))
	fmt.Printf("Evidence:          PRE-BAKED — no cluster and no Relay were involved\n")

	r.printAttribution()
	r.printTranscript()
	r.printResult()
	r.printFailures()
	r.printScoring()
}

// printAttribution is the provider and model that actually answered, per call.
func (r *report) printAttribution() {
	r.section("ATTRIBUTION, TOKENS, COST, LATENCY, CACHE")
	if len(r.records) == 0 {
		fmt.Println("No call reached a provider.")
		return
	}

	var totals reasoning.TokenUsage
	var cost int64
	var latency time.Duration
	for _, entry := range r.records {
		fmt.Printf("\n[%s] %s → answered by %s\n",
			entry.Method, entry.RequestedModel, entry.AnsweringModel)
		fmt.Printf("  request id:      %s\n", orNone(entry.RequestID))
		fmt.Printf("  stop reason:     %s\n", stopText(entry.Stop))
		fmt.Printf("  input tokens:    %s\n", count(entry.Usage.Input))
		fmt.Printf("  output tokens:   %s\n", count(entry.Usage.Output))
		fmt.Printf("  cache write:     %s\n", count(entry.Usage.CacheWrite))
		fmt.Printf("  cache read:      %s\n", count(entry.Usage.CacheRead))
		fmt.Printf("  reasoning:       %s\n", count(entry.Usage.Reasoning))
		fmt.Printf("  cost:            %s\n", money(entry.MicroCents))
		fmt.Printf("  latency:         %s\n", entry.Latency.Round(time.Millisecond))
		if entry.FellBack {
			fmt.Printf("  FELL BACK FROM:  %s\n", entry.FellBackFrom)
		}

		totals.Input = add(totals.Input, entry.Usage.Input)
		totals.Output = add(totals.Output, entry.Usage.Output)
		totals.CacheWrite = add(totals.CacheWrite, entry.Usage.CacheWrite)
		totals.CacheRead = add(totals.CacheRead, entry.Usage.CacheRead)
		totals.Reasoning = add(totals.Reasoning, entry.Usage.Reasoning)
		cost += entry.MicroCents
		latency += entry.Latency
	}

	fmt.Printf("\nTOTAL over %d call(s)\n", len(r.records))
	fmt.Printf("  input %s, output %s, cache write %s, cache read %s, reasoning %s\n",
		count(totals.Input), count(totals.Output), count(totals.CacheWrite),
		count(totals.CacheRead), count(totals.Reasoning))
	fmt.Printf("  billable tokens: %d\n", totals.Billable())
	fmt.Printf("  cost:            %s\n", money(cost))
	fmt.Printf("  provider time:   %s\n", latency.Round(time.Millisecond))

	// Cache effectiveness, measured rather than assumed. A cache that silently stopped working
	// looks exactly like one that is working unless both figures are reported.
	fmt.Printf("\nCACHE EFFECTIVENESS\n")
	switch {
	case !r.support.Caching:
		fmt.Println("  This provider does not report caching, so there is nothing to measure.")
	case !totals.CacheRead.Reported && !totals.CacheWrite.Reported:
		fmt.Println("  The provider reported neither figure. This is UNMEASURED, not zero.")
	default:
		cacheable := totals.CacheRead.Or(0) + totals.CacheWrite.Or(0) + totals.Input.Or(0)
		hitRate := 0.0
		if cacheable > 0 {
			hitRate = float64(totals.CacheRead.Or(0)) / float64(cacheable) * 100
		}
		fmt.Printf("  %d tokens written to cache, %d read from it (%.1f%% of input served warm)\n",
			totals.CacheWrite.Or(0), totals.CacheRead.Or(0), hitRate)
		if totals.CacheRead.Or(0) == 0 && len(r.records) > 1 {
			fmt.Println("  NOTHING was read from cache across several calls. Either the prefix " +
				"is under this provider's minimum cacheable size, or something volatile is " +
				"moving inside it.")
		}
	}
}

// printTranscript is what the model actually said, which is the thing being scored.
func (r *report) printTranscript() {
	r.section("THE TRANSCRIPT")

	fmt.Println("HYPOTHESES PROPOSED FROM THE BRIEF ALONE")
	if len(r.hypotheses) == 0 {
		fmt.Println("  (none)")
	}
	for _, hypothesis := range r.hypotheses {
		fmt.Printf("  %d. %s\n     disproved by: %s\n",
			hypothesis.Ordinal, hypothesis.Statement, hypothesis.Falsifies)
	}

	fmt.Println("\nREADS PROPOSED")
	if len(r.proposals) == 0 {
		fmt.Println("  (none)")
	}
	for index, proposal := range r.proposals {
		fmt.Printf("  %d. %s (hypothesis %d)\n     because: %s\n",
			index+1, proposal.CapabilityID, proposal.Justification, proposal.Reason)
		if proposal.Arguments.PodName != "" {
			fmt.Printf("     pod %s, container %s, previous instance %t\n",
				proposal.Arguments.PodName, proposal.Arguments.ContainerName,
				proposal.Arguments.Previous)
		}
	}
	for _, refusal := range r.refusedReads {
		fmt.Printf("  REFUSED BEFORE DISPATCH: %s\n", refusal)
	}

	fmt.Printf("\nEVIDENCE SERVED (%d items)\n", len(r.served))
	for _, item := range r.served {
		fmt.Printf("  %d. %s\n", item.Ordinal, item.Statement)
	}

	fmt.Println("\nHOW THE EVIDENCE WAS WEIGHED")
	if len(r.weighings) == 0 {
		fmt.Println("  (nothing was weighed)")
	}
	for _, weighed := range r.weighings {
		fmt.Printf("  hypothesis %d vs evidence %d: %s — %s\n",
			weighed.Hypothesis, weighed.Evidence, weighed.Stance, weighed.Reason)
	}

	fmt.Println("\nHYPOTHESES SETTLED")
	if len(r.settlings) == 0 {
		fmt.Println("  (none settled)")
	}
	for _, settled := range r.settlings {
		fmt.Printf("  hypothesis %d → %s: %s\n",
			settled.Hypothesis, settled.State, settled.Reason)
	}
}

// printResult is what the investigation concluded, after the output schema had its say.
func (r *report) printResult() {
	r.section("THE INVESTIGATION RESULT")
	if r.draft.Kind == 0 {
		fmt.Println("No draft outcome was produced.")
		return
	}

	fmt.Printf("Kind:      %s\n", r.draft.Kind)
	fmt.Printf("Statement: %s\n\n", r.draft.Statement)
	for index, claim := range r.draft.Claims {
		fmt.Printf("  claim %d [%s] %s\n     cites evidence %v\n",
			index+1, claim.Role, claim.Statement, claim.Evidence)
	}
	if len(r.draft.RelevantGaps) > 0 {
		fmt.Printf("\n  coverage gaps that mattered: %v\n", r.draft.RelevantGaps)
	}
	if len(r.draft.Unresolved) > 0 {
		fmt.Printf("  hypotheses left unresolved:  %v\n", r.draft.Unresolved)
	}

	if r.outcome != nil {
		fmt.Printf("\nADMITTED by the output schema. %d claim(s) across %d independent source(s).\n",
			len(r.outcome.Claims), r.outcome.IndependentSources)
	}
}

// printFailures is every refusal, retry, fallback and contract failure the run saw.
func (r *report) printFailures() {
	r.section("REFUSALS, RETRIES, FALLBACKS AND CONTRACT FAILURES")

	// A retry is a second call for the same method: a schema-invalid answer is retried exactly
	// once, inside the deployment that produced it. Counting extra calls against the three the
	// boundary makes would misreport any run that ended early.
	perMethod := make(map[string]int, len(r.records))
	for _, entry := range r.records {
		perMethod[entry.Method]++
	}
	retries := 0
	for _, calls := range perMethod {
		retries += calls - 1
	}
	if retries > 0 {
		fmt.Printf("RETRIES: %d — an answer did not satisfy the schema and was asked again. "+
			"The retry is bounded at one per call.\n", retries)
	} else {
		fmt.Println("Retries:            none — every answer that arrived satisfied the schema.")
	}

	refusals, fallbacks := 0, 0
	for _, entry := range r.records {
		if entry.Stop == reasoning.StopRefused {
			refusals++
		}
		if entry.FellBack {
			fallbacks++
		}
	}
	fmt.Printf("Provider refusals:  %d\n", refusals)
	fmt.Printf("Fallbacks taken:    %d\n", fallbacks)
	fmt.Printf("Reads refused:      %d (refused by scope validation before dispatch)\n",
		len(r.refusedReads))

	if r.admissionFailure != nil {
		fmt.Printf("\nCONTRACT FAILURE — the output schema refused the draft:\n  %v\n",
			r.admissionFailure)
		if errors.Is(r.admissionFailure, investigation.ErrUncited) {
			fmt.Println("  This is the uncited-claim rule firing. The draft did not reach storage.")
		}
	}
	if r.failed != nil {
		fmt.Printf("\nTHE RUN DID NOT COMPLETE:\n  %v\n", r.failed)
		if outcome, named := reasoning.OutcomeOf(r.failed); named {
			fmt.Printf("  Named outcome: %s\n", outcome)
		}
	}
	if r.admissionFailure == nil && r.failed == nil {
		fmt.Println("\nNo contract or prompt failure.")
	}
}

// printScoring is the human's part. The questions are fixed so two runs are scored the same way.
func (r *report) printScoring() {
	r.section("HUMAN SCORING (the program does not answer these)")
	fmt.Println(`The cause written down before the model saw anything: a rotated database
credential. The distractor: an image update thirty minutes earlier.

  1. CONCLUSION. Did it reach the credential rotation, blame the deploy, or abstain?
     Blaming the deploy is being led by proximity. Abstaining is over-cautious: the
     evidence does point somewhere.

  2. DISCRIMINATION. Did it notice that the pod on the PREVIOUS image fails identically?
     That is the fact that takes the deploy out of the running, and using it is the
     difference between reasoning and pattern-matching.

  3. EVIDENCE SELECTION, scored apart from the conclusion. Were the reads it chose the
     ones that would discriminate between its own hypotheses, or were they confirmation?

  4. CITATION. Does every claim rest on evidence that actually says what the claim says?

  5. CONTRADICTION. Was anything that pointed the other way reported rather than
     quietly dropped?

  6. WOULD AN ENGINEER HAVE BEEN FASTER WITHOUT IT?`)
}

func (r *report) section(title string) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("=", len(title)))
}

// stopText renders why generation ended, saying so plainly when it never began. The zero value
// means the call failed before the provider reported anything, which is not the same as a stop
// reason this build does not recognise.
func stopText(stop reasoning.Stop) string {
	if stop == 0 {
		return "the provider never answered"
	}
	return stop.String()
}

// count renders a token figure, keeping absent distinguishable from zero.
func count(value reasoning.Count) string {
	if !value.Reported {
		return "not reported by this provider"
	}
	return fmt.Sprintf("%d", value.Tokens)
}

// add sums two figures, staying unreported unless at least one side was measured.
func add(running, next reasoning.Count) reasoning.Count {
	if !next.Reported {
		return running
	}
	return reasoning.Counted(running.Or(0) + next.Tokens)
}

// money renders integer micro-cents as something a person can read, without ever turning the
// stored figure into a float.
func money(microCents int64) string {
	return fmt.Sprintf("%d micro-cents (about $%d.%06d)",
		microCents, microCents/100_000_000, (microCents%100_000_000)/100)
}

func orNone(value string) string {
	if value == "" {
		return "(none reported)"
	}
	return value
}

package reasoning

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// THE PEACETIME ANSWER.
//
// A whole class of real SRE question — "which version is currently deployed?" — has a
// direct answer and no cause to name. These assert the two things that make one
// expressible: the answer field the conclusion carries, and the observation kind a fact
// with no causal role is stated under.

func TestAConclusionCarriesTheDirectAnswer(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{
		toolCallCompletion(t, "call-1", "slack.list_channels", map[string]any{}),
		concludeCompletion(t, map[string]any{
			"answer": "checkout-api is running v2.14.1.",
			"findings": []map[string]any{{
				"statement":  "the deployed revision of checkout-api is v2.14.1",
				"kind":       "observation",
				"confidence": "confirmed",
				"sources":    []int{1},
			}},
			"next_steps": []string{},
		}),
	}}
	exchange, err := agentWith(t, provider).OpenExchange(
		context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exchange.Next(context.Background(), nil, false, ""); err != nil {
		t.Fatal(err)
	}

	move, err := exchange.Next(context.Background(),
		[]investigation.CallResult{succeededResult("call-1", 1)}, false, "")
	if err != nil {
		t.Fatal(err)
	}

	if move.Conclusion == nil {
		t.Fatalf("move = %+v", move)
	}
	if move.Conclusion.Summary != "checkout-api is running v2.14.1." {
		t.Errorf("answer = %q; the direct reply is what a question is owed",
			move.Conclusion.Summary)
	}
	if len(move.Conclusion.Findings) != 1 ||
		move.Conclusion.Findings[0].Kind != investigation.FindingObservation {
		t.Errorf("findings = %+v; a fact with no causal role is an observation",
			move.Conclusion.Findings)
	}
	// The answer summarises; the finding still carries the claim and its citation.
	if len(move.Conclusion.Findings[0].Sources) != 1 {
		t.Errorf("finding = %+v; the citation invariant is unchanged by the answer",
			move.Conclusion.Findings[0])
	}
}

// An answer past the bound is ACCEPTED here and bounded where it is persisted.
//
// This reverses a rule that used to hold. It was justified by the record's own CHECK
// refusing the write — and the record has no such CHECK: question, subject and error each
// carry a length constraint, the answer does not, because it sits inside the conclusion's
// JSONB. So the bound was never the database's, and refusing the conclusion for it threw
// away every read that had already succeeded. On 2026-08-22 that destroyed a live
// investigation which had correctly read five sources, purely because its answer ran long.
func TestAnAnswerPastItsBoundIsNotMalformed(t *testing.T) {
	t.Parallel()

	oversized := concludeCompletion(t, map[string]any{
		"answer":     strings.Repeat("x", investigation.MaxSummaryLength+1),
		"findings":   []map[string]any{},
		"next_steps": []string{},
	})
	provider := &fakeProvider{completions: []Completion{oversized, oversized}}

	exchange, err := agentWith(t, provider).OpenExchange(
		context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}
	move, err := exchange.Next(context.Background(), nil, true, "over")
	if err != nil {
		t.Fatalf("err = %v; length is not malformation, and failing here discards "+
			"every read that succeeded", err)
	}
	if move.Conclusion == nil {
		t.Fatalf("move = %+v; the conclusion was discarded for being long", move)
	}
}

// Every investigation owes an operator-facing summary, including an incident turn.
func TestAnIncidentConclusionCarriesASummary(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{
		concludeCompletion(t, map[string]any{
			"answer":     "",
			"findings":   []map[string]any{},
			"next_steps": []string{},
		}),
	}}
	exchange, err := agentWith(t, provider).OpenExchange(
		context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}

	move, err := exchange.Next(context.Background(), nil, true, "over")
	if err != nil {
		t.Fatalf("err = %v; an incident owes no direct answer", err)
	}
	if move.Conclusion == nil || move.Conclusion.Summary == "" {
		t.Errorf("conclusion = %+v", move.Conclusion)
	}
}

// The conclude tool has to OFFER the answer for a model to fill it in. A field enforced
// on the way in but absent from the contract is a field nothing ever sends.
func TestTheConcludeToolOffersTheStructuredFieldsAndTheObservationKind(t *testing.T) {
	t.Parallel()

	schema := ConcludeDefinition().InputSchema
	document, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the conclude schema has no properties: %+v", schema)
	}
	for _, field := range []string{"status", "summary", "impact", "hypotheses", "actions", "limitations"} {
		if _, offered := document[field]; !offered {
			t.Errorf("the conclude document does not offer %q: %+v", field, document)
		}
	}

	findings, ok := document["findings"].(map[string]any)
	if !ok {
		t.Fatalf("findings is not an array schema: %+v", document["findings"])
	}
	items, ok := findings["items"].(map[string]any)
	if !ok {
		t.Fatalf("findings has no item schema: %+v", findings)
	}
	properties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("a finding has no properties: %+v", items)
	}
	kind, ok := properties["kind"].(map[string]any)
	if !ok {
		t.Fatalf("a finding has no kind: %+v", properties)
	}
	allowed, ok := kind["enum"].([]any)
	if !ok {
		t.Fatalf("kind is not an enum: %+v", kind)
	}
	for _, value := range allowed {
		if value == investigation.FindingObservation {
			return
		}
	}
	t.Errorf("the kind enum does not offer %q: %+v",
		investigation.FindingObservation, allowed)
}

// A turn that came from a QUESTION owes a reply. An empty answer is malformed, which the
// decode path retries once — the same model usually answers the second time. Tolerating
// silence would hand somebody who asked a question a causal-findings document and no reply.
func TestAQuestionTurnMustAnswer(t *testing.T) {
	t.Parallel()

	silent := concludeCompletion(t, map[string]any{
		"answer":     "   ",
		"findings":   []map[string]any{},
		"next_steps": []string{},
	})
	provider := &fakeProvider{completions: []Completion{silent, silent}}

	asked := testOrientation()
	asked.Question = "which version is currently deployed?"
	exchange, err := agentWith(t, provider).OpenExchange(context.Background(), asked)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = exchange.Next(
		context.Background(), nil, true, "over"); err == nil || !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v; a question concluded with no answer is malformed", err)
	}
	if len(provider.prompts) != 2 {
		t.Errorf("attempts = %d, want one retry before giving up", len(provider.prompts))
	}
}

// An incident turn still owes a concise summary even though no operator asked a question.
func TestAnIncidentTurnMustSummarize(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{completions: []Completion{
		concludeCompletion(t, map[string]any{
			"answer":     "",
			"findings":   []map[string]any{},
			"next_steps": []string{},
		}),
	}}

	fromIncident := testOrientation()
	fromIncident.Question = ""
	exchange, err := agentWith(t, provider).OpenExchange(context.Background(), fromIncident)
	if err != nil {
		t.Fatal(err)
	}

	if move, err := exchange.Next(context.Background(), nil, true, "over"); err != nil {
		t.Fatalf("err = %v; the incident summary should be accepted", err)
	} else if move.Conclusion == nil || move.Conclusion.Summary == "" {
		t.Fatalf("conclusion = %+v; the incident owes an operator summary", move.Conclusion)
	}
}

// THE OVERSIZED ANSWER.
//
// An answer past the bound is well formed and too long, which is not malformed output.
// Failing the investigation for it discards every read that already succeeded — a run
// that read five sources correctly is destroyed at the last step — so the decoder accepts
// it and the single place that persists the answer is what bounds it.
func TestAnAnswerPastTheBoundDoesNotFailTheInvestigation(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", investigation.MaxSummaryLength+1000)
	provider := &fakeProvider{completions: []Completion{
		toolCallCompletion(t, "call-1", "slack.list_channels", map[string]any{}),
		concludeCompletion(t, map[string]any{
			"answer": long,
			"findings": []map[string]any{{
				"statement":  "the deployed revision of checkout-api is v2.14.1",
				"kind":       "observation",
				"confidence": "confirmed",
				"sources":    []int{1},
			}},
			"next_steps": []string{},
		}),
	}}
	exchange, err := agentWith(t, provider).OpenExchange(
		context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exchange.Next(context.Background(), nil, false, ""); err != nil {
		t.Fatal(err)
	}

	move, err := exchange.Next(context.Background(),
		[]investigation.CallResult{succeededResult("call-1", 1)}, false, "")
	if err != nil {
		t.Fatalf("an over-length answer failed the investigation: %v", err)
	}
	if move.Conclusion == nil {
		t.Fatalf("move = %+v; the conclusion was discarded for being long", move)
	}
	if move.Conclusion.Summary != long {
		t.Error("the decoder altered the answer; bounding belongs where it is persisted")
	}
}

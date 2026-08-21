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
	if move.Conclusion.Answer != "checkout-api is running v2.14.1." {
		t.Errorf("answer = %q; the direct reply is what a question is owed",
			move.Conclusion.Answer)
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

// An answer past the record's bound is malformed rather than silently cut. The schema
// deliberately does not express bounds — several providers drop them — so this is the
// only place the limit holds before the record's own CHECK would refuse the write.
func TestAnAnswerPastItsBoundIsMalformed(t *testing.T) {
	t.Parallel()

	oversized := concludeCompletion(t, map[string]any{
		"answer":     strings.Repeat("x", investigation.MaxAnswerLength+1),
		"findings":   []map[string]any{},
		"next_steps": []string{},
	})
	provider := &fakeProvider{completions: []Completion{oversized, oversized}}

	exchange, err := agentWith(t, provider).OpenExchange(
		context.Background(), testOrientation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exchange.Next(
		context.Background(), nil, true, "over"); err == nil || !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v; an answer the record cannot hold is malformed", err)
	}
}

// An episode-triggered investigation is not asked anything, so it owes no answer. The
// findings are the answer, and requiring prose beside them would be requiring the model
// to say something twice.
func TestAConclusionWithoutAnAnswerIsWellFormed(t *testing.T) {
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
		t.Fatalf("err = %v; an episode owes no direct answer", err)
	}
	if move.Conclusion == nil || move.Conclusion.Answer != "" {
		t.Errorf("conclusion = %+v", move.Conclusion)
	}
}

// The conclude tool has to OFFER the answer for a model to fill it in. A field enforced
// on the way in but absent from the contract is a field nothing ever sends.
func TestTheConcludeToolOffersTheAnswerAndTheObservationKind(t *testing.T) {
	t.Parallel()

	schema := ConcludeDefinition().InputSchema
	document, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the conclude schema has no properties: %+v", schema)
	}
	if _, offered := document["answer"]; !offered {
		t.Errorf("the conclude document does not offer an answer: %+v", document)
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

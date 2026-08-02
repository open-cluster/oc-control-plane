package reasoning_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// THE CONFIGURED FALLBACK CHAIN.
//
// A vendor is never switched silently. Crossing to the next hop happens only for an outcome another
// deployment could plausibly answer, only where that hop is consented to, and it is always recorded
// — so the record always says who actually replied.

// chain builds a service over the given providers and captures every record it publishes.
func chain(
	t *testing.T, consented []string, ceiling *reasoning.Ceiling, providers ...*fakeProvider,
) (*reasoning.Service, *records) {
	t.Helper()

	captured := &records{}
	deployments := make([]reasoning.Deployment, 0, len(providers))
	for index, provider := range providers {
		model := "model-a"
		if index > 0 {
			model = "model-b"
		}
		deployments = append(deployments, reasoning.Deployment{
			Provider:   provider.Name(),
			Model:      model,
			Effort:     reasoning.EffortHigh,
			Credential: reasoning.Secret("test-credential-value"),
		})
	}

	options := reasoning.Options{
		Primary:     providers[0],
		Deployments: deployments,
		Tariff:      testTariff(),
		Consent:     reasoning.ConsentTo(consented...),
		Ceiling:     ceiling,
		Now:         steadyClock(),
		Observe:     captured.add,
	}
	for _, provider := range providers[1:] {
		options.Fallbacks = append(options.Fallbacks, provider)
	}

	service, err := reasoning.New(options)
	if err != nil {
		t.Fatalf("building the reasoning service: %v", err)
	}
	return service, captured
}

type records struct {
	mutex   sync.Mutex
	entries []reasoning.Record
}

func (r *records) add(entry reasoning.Record) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.entries = append(r.entries, entry)
}

func (r *records) all() []reasoning.Record {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]reasoning.Record(nil), r.entries...)
}

func TestChain_CrossesToTheNextDeploymentOnAnOutcomeAnotherCouldAnswer(t *testing.T) {
	recoverable := map[string]reasoning.Outcome{
		"a refusal": reasoning.OutcomeRefused,
		"an outage": reasoning.OutcomeOutage,
		"a timeout": reasoning.OutcomeTimeout,
	}
	for name, outcome := range recoverable {
		t.Run(name, func(t *testing.T) {
			primary := newFakeProvider("primary", answer{
				err:   reasoning.Failed(outcome, "primary", "model-a", "gave way"),
				usage: usageOf(500, 0, 0, 0),
			})
			fallback := newFakeProvider("fallback", answer{
				document: goodHypotheses,
				usage:    usageOf(1000, 200, 0, 0),
			})
			service, captured := chain(t, []string{"primary", "fallback"}, nil, primary, fallback)

			proposed, err := service.Hypotheses(context.Background(), briefFixture())
			if err != nil {
				t.Fatalf("the chain did not recover the round: %v", err)
			}
			if len(proposed.Hypotheses) != 2 {
				t.Errorf("got %d hypotheses from the fallback, want 2", len(proposed.Hypotheses))
			}

			entries := captured.all()
			if len(entries) != 2 {
				t.Fatalf("got %d records, want one per hop", len(entries))
			}
			if entries[0].Provider != "primary" || entries[0].FellBack {
				t.Errorf("the first record is wrong: %+v", entries[0])
			}
			// Never silently: the hop that answered records that it was a fallback and what it
			// gave way from.
			if !entries[1].FellBack {
				t.Error("the answering hop does not record that it was a fallback")
			}
			if entries[1].FellBackFrom != "primary/model-a" {
				t.Errorf("the answering hop fell back from %q, want primary/model-a",
					entries[1].FellBackFrom)
			}
			if entries[1].AnsweringModel != "model-b" {
				t.Errorf("the record names %q as answering, want model-b",
					entries[1].AnsweringModel)
			}
		})
	}
}

func TestChain_DoesNotCrossOnARejectedRequest(t *testing.T) {
	// A rejected request is this build's own defect. Every hop would reject it identically, so
	// trying the next one spends money to fail again.
	primary := newFakeProvider("primary", answer{
		err: reasoning.Failed(reasoning.OutcomeRejected, "primary", "model-a", "bad request"),
	})
	fallback := newFakeProvider("fallback", answer{document: goodHypotheses})
	service, _ := chain(t, []string{"primary", "fallback"}, nil, primary, fallback)

	_, err := service.Hypotheses(context.Background(), briefFixture())
	if !errors.Is(err, reasoning.ErrRejected) {
		t.Fatalf("got %v, want the rejected request to end the round", err)
	}
	if fallback.callCount() != 0 {
		t.Errorf("the fallback was asked %d times, want 0", fallback.callCount())
	}
}

func TestChain_DoesNotCrossToAProviderNobodyConsentedTo(t *testing.T) {
	primary := newFakeProvider("primary", answer{
		err: reasoning.Failed(reasoning.OutcomeOutage, "primary", "model-a", "unreachable"),
	})
	fallback := newFakeProvider("fallback", answer{document: goodHypotheses})

	// Consent is checked PER HOP. Consenting to the primary is not consenting to the fallback: a
	// hop that inherited it would be an undisclosed subprocessor change performed automatically.
	service, _ := chain(t, []string{"primary", "fallback"}, nil, primary, fallback)
	if _, err := service.Hypotheses(context.Background(), briefFixture()); err != nil {
		t.Fatalf("the consented chain should have answered: %v", err)
	}
	if fallback.callCount() != 1 {
		t.Fatalf("the consented fallback was asked %d times, want 1", fallback.callCount())
	}

	// The same chain with consent withdrawn from the second hop must not reach it.
	primaryAgain := newFakeProvider("primary", answer{
		err: reasoning.Failed(reasoning.OutcomeOutage, "primary", "model-a", "unreachable"),
	})
	unconsented := newFakeProvider("fallback", answer{document: goodHypotheses})
	withheld, err := reasoning.New(reasoning.Options{
		Primary:   primaryAgain,
		Fallbacks: []reasoning.Provider{unconsented},
		Deployments: []reasoning.Deployment{
			{Provider: "primary", Model: "model-a", Effort: reasoning.EffortHigh,
				Credential: reasoning.Secret("test-credential-value")},
			{Provider: "fallback", Model: "model-b", Effort: reasoning.EffortHigh,
				Credential: reasoning.Secret("test-credential-value")},
		},
		Tariff: testTariff(),
		// Only the primary is consented to.
		Consent: reasoning.ConsentTo("primary"),
		Now:     steadyClock(),
	})
	// A provider configured but not consented to is refused at STARTUP, which is the earliest
	// anyone can be told.
	if !errors.Is(err, reasoning.ErrNotConsented) {
		t.Fatalf("got %v building a service with an unconsented hop, want a consent refusal", err)
	}
	if withheld != nil {
		t.Error("a service was built over a provider nobody consented to")
	}
	if unconsented.callCount() != 0 {
		t.Errorf("the unconsented provider was asked %d times, want 0", unconsented.callCount())
	}
}

func TestChain_AReachedCostCeilingStopsTheChainRatherThanMovingDownIt(t *testing.T) {
	// A cheaper model is still spending money that has run out.
	ceiling := reasoning.NewCeiling(1)
	ceiling.Record(5_000)

	primary := newFakeProvider("primary", answer{document: goodHypotheses})
	fallback := newFakeProvider("fallback", answer{document: goodHypotheses})
	service, _ := chain(t, []string{"primary", "fallback"}, ceiling, primary, fallback)

	_, err := service.Hypotheses(context.Background(), briefFixture())
	if !errors.Is(err, reasoning.ErrCeilingReached) {
		t.Fatalf("got %v, want the cost ceiling to stop the round", err)
	}
	if primary.callCount() != 0 || fallback.callCount() != 0 {
		t.Errorf("a request was sent past a reached cost ceiling: primary %d, fallback %d",
			primary.callCount(), fallback.callCount())
	}
}

func TestChain_ARefusedCallStillRecordsWhatItSpent(t *testing.T) {
	// A refused request that consumed input tokens spent real money. A budget that only counted
	// successes would be one an unlucky day could walk straight through.
	primary := newFakeProvider("primary", answer{
		err:   reasoning.Failed(reasoning.OutcomeRefused, "primary", "model-a", "declined"),
		usage: usageOf(10_000, 0, 0, 0),
	})
	service, captured := chain(t, []string{"primary"}, nil, primary)

	if _, err := service.Hypotheses(context.Background(), briefFixture()); err == nil {
		t.Fatal("a refusal did not fail the round")
	}

	entries := captured.all()
	if len(entries) != 1 {
		t.Fatalf("got %d records for a refused call, want 1", len(entries))
	}
	if entries[0].Usage.Input.Or(0) != 10_000 {
		t.Errorf("the refused call recorded %d input tokens, want 10000",
			entries[0].Usage.Input.Or(0))
	}
	if entries[0].MicroCents == 0 {
		t.Error("the refused call was costed at zero despite consuming input tokens")
	}
	if entries[0].RequestID == "" {
		t.Error("the refused call recorded no provider request identifier")
	}
}

func TestRecord_CarriesEveryFieldARoundIsAttributedBy(t *testing.T) {
	primary := newFakeProvider("primary", answer{
		document: goodHypotheses,
		usage: reasoning.TokenUsage{
			Input:      reasoning.Counted(1000),
			Output:     reasoning.Counted(200),
			CacheWrite: reasoning.Counted(300),
			CacheRead:  reasoning.Counted(4000),
			Reasoning:  reasoning.Counted(120),
		},
		model: "model-a-actually-answered",
	})
	service, captured := chain(t, []string{"primary"}, nil, primary)

	if _, err := service.Hypotheses(context.Background(), briefFixture()); err != nil {
		t.Fatalf("proposing hypotheses: %v", err)
	}

	entries := captured.all()
	if len(entries) != 1 {
		t.Fatalf("got %d records, want 1", len(entries))
	}
	entry := entries[0]

	if entry.Provider != "primary" {
		t.Errorf("provider is %q", entry.Provider)
	}
	if entry.RequestedModel != "model-a" {
		t.Errorf("requested model is %q, want model-a", entry.RequestedModel)
	}
	// The model that ANSWERED, read from the response rather than echoed from the request.
	if entry.AnsweringModel != "model-a-actually-answered" {
		t.Errorf("answering model is %q, want what the response said", entry.AnsweringModel)
	}
	if entry.RequestID == "" {
		t.Error("no provider request identifier was recorded")
	}
	if entry.Usage.Input.Or(0) != 1000 || entry.Usage.Output.Or(0) != 200 {
		t.Errorf("input and output tokens are wrong: %+v", entry.Usage)
	}
	if entry.Usage.CacheWrite.Or(0) != 300 || entry.Usage.CacheRead.Or(0) != 4000 {
		t.Errorf("cache tokens are wrong: %+v", entry.Usage)
	}
	if !entry.Usage.Reasoning.Reported || entry.Usage.Reasoning.Tokens != 120 {
		t.Errorf("reasoning tokens are wrong: %+v", entry.Usage.Reasoning)
	}
	if entry.FellBack {
		t.Error("a first-hop answer is recorded as a fallback")
	}
	if entry.Stop != reasoning.StopComplete {
		t.Errorf("stop reason is %s", entry.Stop)
	}
	if entry.MicroCents <= 0 {
		t.Error("the call was costed at zero")
	}
	if entry.Latency <= 0 {
		t.Error("no latency was recorded")
	}
	if entry.Method != "hypotheses" {
		t.Errorf("method is %q, want hypotheses", entry.Method)
	}
	if entry.PromptVersion != reasoning.PromptVersion ||
		entry.SchemaVersion != reasoning.SchemaVersion {
		t.Errorf("prompt and schema versions are wrong: %q %q",
			entry.PromptVersion, entry.SchemaVersion)
	}
}

func TestRecord_AnUnreportedFigureIsAbsentRatherThanZero(t *testing.T) {
	// Zero is a measurement; absent is the lack of one. Collapsing them makes a cache that
	// stopped working indistinguishable from a provider that never reported one.
	primary := newFakeProvider("primary", answer{
		document: goodHypotheses,
		usage: reasoning.TokenUsage{
			Input:      reasoning.Counted(1000),
			Output:     reasoning.Counted(200),
			CacheWrite: reasoning.Unreported(),
			CacheRead:  reasoning.Counted(0),
		},
	})
	service, captured := chain(t, []string{"primary"}, nil, primary)

	if _, err := service.Hypotheses(context.Background(), briefFixture()); err != nil {
		t.Fatalf("proposing hypotheses: %v", err)
	}

	entry := captured.all()[0]
	if entry.Usage.CacheWrite.Reported {
		t.Error("a figure the provider never reported is recorded as measured")
	}
	if !entry.Usage.CacheRead.Reported {
		t.Error("a measured zero is recorded as unreported, which loses a real measurement")
	}
}

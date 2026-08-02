package reasoning_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// The fixtures every test in this package is written against.
//
// There is one brief, one deliberation and one fake provider, because the thing under test is what
// this package sends and what it does with what comes back — not the shape of any particular
// investigation. A test that built its own brief each time would be asserting against a brief
// nobody ships.

// fakeProvider stands in for a vendor. It is the seam above the HTTP round-tripper: the adapters'
// own tests use a transport, and these tests use this, so a failure here is never ambiguous about
// which layer produced it.
type fakeProvider struct {
	name    string
	support reasoning.Support

	mutex sync.Mutex
	// answers is returned in order, one per call. A test that supplies two is testing the retry.
	answers []answer
	// prompts records what was actually sent, so a test can assert over the rendered prompt.
	prompts []reasoning.Prompt
	calls   int
	// measured is what Measure reports, when this provider counts tokens at all.
	measured int64
}

// answer is one canned reply.
type answer struct {
	document string
	usage    reasoning.TokenUsage
	stop     reasoning.Stop
	model    string
	err      error
}

func newFakeProvider(name string, answers ...answer) *fakeProvider {
	return &fakeProvider{
		name: name,
		support: reasoning.Support{
			StrictStructuredOutput: true,
			TokenCounting:          true,
			Streaming:              true,
			Caching:                true,
			RefusalDetection:       true,
		},
		answers: answers,
	}
}

func (f *fakeProvider) Name() string               { return f.name }
func (f *fakeProvider) Support() reasoning.Support { return f.support }

func (f *fakeProvider) Complete(
	_ context.Context, prompt reasoning.Prompt,
) (reasoning.Completion, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.prompts = append(f.prompts, prompt)
	index := f.calls
	f.calls++
	if index >= len(f.answers) {
		index = len(f.answers) - 1
	}
	reply := f.answers[index]

	model := reply.model
	if model == "" {
		model = prompt.Model
	}
	stop := reply.stop
	if stop == 0 {
		stop = reasoning.StopComplete
	}
	return reasoning.Completion{
		Model:     model,
		RequestID: "req_" + f.name,
		Document:  []byte(reply.document),
		Stop:      stop,
		Usage:     reply.usage,
	}, reply.err
}

func (f *fakeProvider) Measure(
	context.Context, reasoning.Prompt,
) (reasoning.Count, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if !f.support.TokenCounting {
		return reasoning.Unreported(), nil
	}
	return reasoning.Counted(f.measured), nil
}

func (f *fakeProvider) callCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.calls
}

func (f *fakeProvider) lastPrompt(t *testing.T) reasoning.Prompt {
	t.Helper()
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if len(f.prompts) == 0 {
		t.Fatal("the provider was never asked for anything")
	}
	return f.prompts[len(f.prompts)-1]
}

// usageOf is a token figure every field of which was reported, which is what a well-behaved
// provider returns.
func usageOf(input, output, cacheWrite, cacheRead int64) reasoning.TokenUsage {
	return reasoning.TokenUsage{
		Input:      reasoning.Counted(input),
		Output:     reasoning.Counted(output),
		CacheWrite: reasoning.Counted(cacheWrite),
		CacheRead:  reasoning.Counted(cacheRead),
		Reasoning:  reasoning.Unreported(),
	}
}

// testTariff prices the fake providers used here. It is separate from the shipped table so that a
// price change in the product does not silently rewrite what these tests assert.
func testTariff() reasoning.Tariff {
	// In micro-cents per million tokens: $1 input, $10 output, $1.25 cache write, $0.10 cache
	// read. The magnitudes are realistic and the ratios are the real ones — a rate small enough to
	// round to nothing would let a costing bug pass as a passing test.
	rate := reasoning.Rate{
		Input:      100_000_000,
		Output:     1_000_000_000,
		CacheWrite: 125_000_000,
		CacheRead:  10_000_000,
	}
	return reasoning.NewTariff(map[string]reasoning.Rate{
		"primary/model-a":  rate,
		"fallback/model-b": rate,
	})
}

// serviceUnder builds a service over the given providers, with everything else at its most
// permissive so a test asserts one thing at a time.
func serviceUnder(t *testing.T, providers ...*fakeProvider) *reasoning.Service {
	t.Helper()
	if len(providers) == 0 {
		t.Fatal("a service needs at least one provider")
	}

	deployments := make([]reasoning.Deployment, 0, len(providers))
	consented := make([]string, 0, len(providers))
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
		consented = append(consented, provider.Name())
	}

	options := reasoning.Options{
		Primary:     providers[0],
		Deployments: deployments,
		Tariff:      testTariff(),
		Consent:     reasoning.ConsentTo(consented...),
		Now:         steadyClock(),
	}
	for _, provider := range providers[1:] {
		options.Fallbacks = append(options.Fallbacks, provider)
	}

	service, err := reasoning.New(options)
	if err != nil {
		t.Fatalf("building the reasoning service: %v", err)
	}
	return service
}

// steadyClock advances a fixed amount per reading, so a latency figure is deterministic and a
// breaker's cooldown is reachable without sleeping.
func steadyClock() func() time.Time {
	var mutex sync.Mutex
	instant := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mutex.Lock()
		defer mutex.Unlock()
		instant = instant.Add(time.Millisecond * 250)
		return instant
	}
}

// briefFixture is one investigation's orientation: a workload with two pods, one recent change and
// one capability that could not be reached.
func briefFixture() investigation.Brief {
	window := investigation.Window{
		Start: time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
	}
	evidence := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	return investigation.Brief{
		Resource: investigation.ResourceIdentity{
			Kind:               "deployment",
			Name:               "checkout",
			Namespace:          "payments",
			UID:                "22222222-2222-2222-2222-222222222222",
			DesiredReplicas:    3,
			ReadyReplicas:      1,
			UpdatedReplicas:    3,
			AvailableReplicas:  1,
			Generation:         7,
			ObservedGeneration: 7,
			ContainerImages:    []string{"registry.example/checkout:1.4.2"},
			Resolved:           true,
		},
		Trigger: investigation.Trigger{Kind: investigation.TriggerManual},
		Window:  window,
		RecentChanges: []investigation.Change{{
			At:       time.Date(2026, 8, 2, 11, 15, 0, 0, time.UTC),
			Summary:  "image updated to checkout:1.4.2",
			Evidence: evidence,
		}},
		Topology: []investigation.TopologyFact{
			{Pod: "checkout-7d9f-aaaaa", Node: "node-1", Owner: "checkout-7d9f",
				Phase: "Running", Ready: true, Evidence: evidence},
			{Pod: "checkout-7d9f-bbbbb", Node: "node-2", Owner: "checkout-7d9f",
				Phase: "CrashLoopBackOff", Ready: false, Evidence: evidence},
		},
		Available: []investigation.CapabilityRef{
			{ID: "kubernetes.workload.runtime", Version: 1},
			{ID: "kubernetes.namespace.events", Version: 1},
			{ID: "kubernetes.container.logs", Version: 1},
		},
		Coverage: []investigation.Coverage{{
			CapabilityID:      "kubernetes.container.logs",
			CapabilityVersion: 1,
			State:             investigation.CoverageUnavailable,
			Reason:            "no read of this capability was made in this round",
		}},
		AssembledAt: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
	}
}

// deliberationFixture is what the reasoner is shown mid-round: two hypotheses, two evidence items
// and one coverage gap.
func deliberationFixture() investigation.Deliberation {
	brief := briefFixture()
	return investigation.Deliberation{
		Brief: brief,
		Hypotheses: []investigation.Hypothesis{
			{
				ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Ordinal: 1,
				Statement: "the new image crashes on startup",
				Falsifies: "a pod running the new image stays ready",
				State:     investigation.HypothesisLive,
			},
			{
				ID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), Ordinal: 2,
				Statement: "the node is evicting the pod",
				Falsifies: "no eviction event names this pod",
				State:     investigation.HypothesisLive,
			},
		},
		Evidence: []investigation.Item{
			{
				ID: uuid.MustParse("55555555-5555-5555-5555-555555555555"), Ordinal: 1,
				CapabilityID: "kubernetes.namespace.events", CapabilityVersion: 1,
				Statement:        "BackOff restarting failed container checkout",
				Content:          "Back-off restarting failed container checkout in pod checkout-7d9f-bbbbb",
				Trust:            investigation.TrustRelayAttested,
				SourceObservedAt: time.Date(2026, 8, 2, 11, 20, 0, 0, time.UTC),
			},
			{
				ID: uuid.MustParse("66666666-6666-6666-6666-666666666666"), Ordinal: 2,
				CapabilityID: "kubernetes.workload.runtime", CapabilityVersion: 1,
				Statement:        "1 of 3 replicas ready",
				Content:          "",
				Trust:            investigation.TrustRelayAttested,
				SourceObservedAt: time.Date(2026, 8, 2, 11, 30, 0, 0, time.UTC),
			},
		},
		Gaps: []investigation.Gap{{
			ID: uuid.MustParse("77777777-7777-7777-7777-777777777777"), Ordinal: 1,
			Cause:       investigation.GapRetentionHorizon,
			Subject:     "the cluster's account of what it did",
			Consequence: "events older than the window cannot be weighed",
		}},
		Available: briefFixture().Available,
		Remaining: 5,
		Pass:      1,
	}
}

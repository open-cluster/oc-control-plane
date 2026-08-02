package reasoning

import (
	"context"
	"sort"
	"sync"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// RECORDING IS A DECORATOR, NOT A FEATURE OF ANY PROVIDER.
//
// It implements the same boundary, delegates every call, and accumulates what was said and what it
// cost. The provider has no idea whether it is being recorded, which is what makes recording a live
// run the same code path in the scenario harness as in a developer's terminal — and what keeps the
// recording honest, because there is nothing a provider could do differently when observed.
//
// What this produces is exactly what the existing replay consumes. A recording made under a
// different prompt or schema version is refused by the transcript key that already exists, so a
// replayed test cannot pass against wording that no longer exists.

// Recorder wraps a reasoner and remembers what it said.
type Recorder struct {
	reasoner investigation.Reasoner

	mutex      sync.Mutex
	hypotheses []investigation.Hypothesis
	passes     map[int]investigation.RecordedPass
	conclusion investigation.RecordedConclusion
	concluded  bool
	spent      investigation.Usage
}

// Recording wraps a reasoner so that a live run writes the transcript commit CI replays.
func Recording(reasoner investigation.Reasoner) *Recorder {
	return &Recorder{
		reasoner: reasoner,
		passes:   make(map[int]investigation.RecordedPass),
	}
}

// Hypotheses delegates and remembers what was proposed.
func (r *Recorder) Hypotheses(
	ctx context.Context, brief investigation.Brief,
) (investigation.Hypothesized, error) {
	proposed, err := r.reasoner.Hypotheses(ctx, brief)

	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.spend(proposed.Usage)
	if err == nil {
		r.hypotheses = proposed.Hypotheses
	}
	return proposed, err
}

// Requests delegates and remembers this pass.
//
// The pass number comes from the deliberation rather than from a counter here, for the same reason
// the replay reads it from there: a counter would interleave two rounds recorded at once into one
// transcript that replays as neither.
func (r *Recorder) Requests(
	ctx context.Context, over investigation.Deliberation,
) (investigation.Proposed, error) {
	proposed, err := r.reasoner.Requests(ctx, over)

	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.spend(proposed.Usage)
	if err == nil {
		r.passes[over.Pass] = investigation.RecordedPass{
			Proposals: proposed.Proposals,
			Weighings: proposed.Weighings,
			Settlings: proposed.Settlings,
		}
	}
	return proposed, err
}

// Conclude delegates and remembers the outcome.
//
// The runner may call this twice, because it retries a response the output schema refused. What is
// kept is the answer that was ACCEPTED — the last one — because a transcript carrying the rejected
// answer would replay a round that fails differently than the one it recorded.
func (r *Recorder) Conclude(
	ctx context.Context, over investigation.Deliberation,
) (investigation.Concluded, error) {
	concluded, err := r.reasoner.Conclude(ctx, over)

	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.spend(concluded.Usage)
	if err == nil {
		r.conclusion = investigation.RecordedConclusion{
			Draft:     concluded.Draft,
			Weighings: concluded.Weighings,
			Settlings: concluded.Settlings,
		}
		r.concluded = true
	}
	return concluded, err
}

// Transcript renders what was recorded, keyed to the components that produced it.
//
// The usage figure is what the run actually cost. A replayed round is free, and recording the
// figure it cost when it was real is what makes a replay comparable to the run it came from.
func (r *Recorder) Transcript(versions investigation.Versions) investigation.Transcript {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// The key names the model that ANSWERED rather than the one that was asked for. They differ
	// when a fallback replied, and a recording keyed on the deployment that gave way would replay
	// against a recording a different model produced — which is the whole failure the key exists
	// to prevent. Where nothing reported an answering model the configured one stands, because a
	// key naming nothing at all would match everything.
	model := versions.Model
	if attributed, reports := r.reasoner.(interface{ Answered() string }); reports {
		if answered := attributed.Answered(); answered != "" {
			model = answered
		}
	}

	transcript := investigation.Transcript{
		Key: investigation.TranscriptKey{
			Model:         model,
			PromptVersion: versions.PromptVersion,
			SchemaVersion: versions.SchemaVersion,
			Investigator:  versions.Investigator,
		},
		Hypotheses: r.hypotheses,
		Conclusion: r.conclusion,
		Usage:      r.spent,
	}

	// Passes are written in order, with any pass that proposed nothing kept as an empty entry.
	// Dropping it would renumber every pass after it, and the replay decides which turn answers a
	// step by the pass number.
	numbers := make([]int, 0, len(r.passes))
	for pass := range r.passes {
		numbers = append(numbers, pass)
	}
	sort.Ints(numbers)
	highest := 0
	if len(numbers) > 0 {
		highest = numbers[len(numbers)-1]
	}
	for pass := 1; pass <= highest; pass++ {
		transcript.Passes = append(transcript.Passes, r.passes[pass])
	}
	return transcript
}

// Concluded reports whether a conclusion was recorded. A transcript without one replays into the
// exhausted error rather than a round, so a caller about to write one to disk should know.
func (r *Recorder) Concluded() bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.concluded
}

// spend accumulates what the run cost. The caller holds the lock.
func (r *Recorder) spend(usage investigation.Usage) {
	r.spent.Tokens += usage.Tokens
	r.spent.MicroCents += usage.MicroCents
}

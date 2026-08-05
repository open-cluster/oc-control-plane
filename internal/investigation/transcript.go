package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The replayed model boundary: what stands in for a provider in commit CI.
//
// Live-model runs are never a commit gate. They belong to the scenario harness, which runs
// manually, before releases, and periodically to detect drift. A CI job that fails on ordinary
// model variance is a gate that gets ignored within two weeks, and once it is ignored it is worse
// than not having it.
//
// A transcript is keyed by the components that produced it and REFUSES to replay for a different
// set. A stale recording that replays silently is a CI run proving the wrong build works, which is
// the failure mode this whole mechanism would otherwise introduce.

// ErrTranscriptExhausted reports a replay that ran past the end of its recording. It is loud rather
// than forgiving: a round that carried on past what was recorded is not the round that was
// recorded, and an answer improvised at that point would be indistinguishable from a real one.
var ErrTranscriptExhausted = errors.New("the recorded transcript has no turn for this step")

// TranscriptKey is what a recording was made against. Every field is part of the identity, because
// changing any of them changes what a real provider would have said.
type TranscriptKey struct {
	Model         string `json:"model"`
	PromptVersion string `json:"promptVersion"`
	SchemaVersion string `json:"schemaVersion"`
	Investigator  string `json:"investigator"`
}

// Matches reports whether a recording may be replayed for these components.
func (k TranscriptKey) Matches(versions Versions) bool {
	return k.Model == versions.Model &&
		k.PromptVersion == versions.PromptVersion &&
		k.SchemaVersion == versions.SchemaVersion &&
		k.Investigator == versions.Investigator
}

func (k TranscriptKey) String() string {
	return fmt.Sprintf("model=%s prompt=%s schema=%s investigator=%s",
		k.Model, k.PromptVersion, k.SchemaVersion, k.Investigator)
}

// Transcript is one recorded run of the model boundary. Its contents are SYNTHETIC scenario data
// only; no customer evidence is ever committed.
type Transcript struct {
	Key TranscriptKey `json:"key"`
	// Hypotheses is what the planner proposed from the brief.
	Hypotheses []Hypothesis `json:"hypotheses"`
	// Passes holds one entry per adaptive pass, in order. A pass with no entry proposes nothing,
	// which is what a planner with nothing further to ask looks like.
	Passes []RecordedPass `json:"passes"`
	// Conclusion is the draft outcome and the last movements of the hypotheses.
	Conclusion RecordedConclusion `json:"conclusion"`
	// Usage is what each turn is recorded as having cost. A replayed round is free; recording the
	// figure it cost when it was real is what makes a replay comparable to the run it came from.
	Usage Usage `json:"usage"`
}

// RecordedPass is one adaptive pass: the reads proposed and what was made of the evidence so far.
type RecordedPass struct {
	Proposals []Proposal `json:"proposals"`
	Weighings []Weighing `json:"weighings"`
	Settlings []Settling `json:"settlings"`
}

// RecordedConclusion is the draft outcome as it was recorded.
type RecordedConclusion struct {
	Draft     Draft      `json:"draft"`
	Weighings []Weighing `json:"weighings"`
	Settlings []Settling `json:"settlings"`
}

// Transcribed is the model boundary for one round, remembering what it answered.
//
// It renders against the round's PINNED versions rather than against whatever the build carries
// now, because the key is what decides whether a recording may be replayed later and a round
// stamped with one set of versions must not file a recording claiming another.
type Transcribed interface {
	Reasoner
	Transcript(versions Versions) Transcript
}

// Transcripts is where a round's recording comes from and where it goes.
//
// It is optional. A deployment that configured nowhere to put recordings gets nil and records
// nothing, which is what every deployment did before this existed. What it closes is that a LIVE
// round previously recorded nothing at all: the transcript file a deployment could be given was
// read only when it was replaying, so a run against a real provider left nothing behind and a
// round that failed could not be read after the fact.
//
// The two methods are one interface because they are one decision. Somewhere to put a recording
// and something that makes one are never configured apart, and splitting them would let a
// deployment hold half of each.
type Transcripts interface {
	// Begin wraps the boundary for ONE round.
	//
	// One recorder per round is the whole of the contract. A recorder shared between two rounds
	// running at once accumulates both into one transcript that replays as neither, and the
	// damage would not be visible until somebody tried to replay it.
	Begin(reasoner Reasoner) Transcribed
	// File writes what a finished round said.
	//
	// It is called for every round however it ended, because a round that FAILED is the one most
	// worth reading — the stage it reached is in what it managed to record — and a corpus that
	// held only the rounds that concluded would be missing exactly the ones somebody goes looking
	// for.
	File(ctx context.Context, of Claimed, transcript Transcript) error
}

// Recorded replays a transcript.
//
// It is stateless: which turn answers a step is decided by the step itself — the brief for
// hypotheses, the pass number for reads, the conclusion for the end — rather than by a counter.
// That is what lets one Recorded serve several rounds at once without them interleaving, and it
// means a replay of one round is identical however many others are running beside it.
type Recorded struct {
	transcript Transcript
}

// Replay returns a reasoner that replays this transcript for these components, refusing outright
// when the recording was made against different ones.
func Replay(transcript Transcript, versions Versions) (*Recorded, error) {
	if !transcript.Key.Matches(versions) {
		return nil, fmt.Errorf("%w: recorded for %s, asked for %s",
			ErrTranscriptKeyMismatch, transcript.Key, TranscriptKey{
				Model:         versions.Model,
				PromptVersion: versions.PromptVersion,
				SchemaVersion: versions.SchemaVersion,
				Investigator:  versions.Investigator,
			})
	}
	return &Recorded{transcript: transcript}, nil
}

// LoadTranscript reads a recording from its stored form.
func LoadTranscript(encoded []byte, versions Versions) (*Recorded, error) {
	var transcript Transcript
	if err := json.Unmarshal(encoded, &transcript); err != nil {
		return nil, fmt.Errorf("reading a transcript: %w", err)
	}
	return Replay(transcript, versions)
}

// Hypotheses replays what the planner proposed from the brief.
func (r *Recorded) Hypotheses(_ context.Context, _ Brief) (Hypothesized, error) {
	proposed := make([]Hypothesis, 0, len(r.transcript.Hypotheses))
	for index, hypothesis := range r.transcript.Hypotheses {
		if hypothesis.State == 0 {
			hypothesis.State = HypothesisLive
		}
		hypothesis.Ordinal = index + 1
		proposed = append(proposed, hypothesis)
	}
	return Hypothesized{Hypotheses: proposed, Usage: r.transcript.Usage}, nil
}

// Requests replays the reads recorded for this pass. A pass beyond the recording proposes nothing,
// which is a planner with nothing further to ask rather than an error: a recording that covered two
// passes is a legitimate recording of a round that only needed two.
func (r *Recorded) Requests(_ context.Context, over Deliberation) (Proposed, error) {
	if over.Pass < 1 || over.Pass > len(r.transcript.Passes) {
		return Proposed{}, nil
	}
	pass := r.transcript.Passes[over.Pass-1]
	return Proposed{
		Proposals: pass.Proposals,
		Weighings: pass.Weighings,
		Settlings: pass.Settlings,
		Usage:     r.transcript.Usage,
	}, nil
}

// Conclude replays the recorded outcome.
//
// It answers the same way on a retry. That is deliberate: the runner retries a response the output
// schema refused, and a recording that produced a different answer the second time would let a
// transcript hide the very rejection it was written to demonstrate.
func (r *Recorded) Conclude(_ context.Context, _ Deliberation) (Concluded, error) {
	if r.transcript.Conclusion.Draft.Kind == 0 {
		return Concluded{}, fmt.Errorf("%w: no conclusion was recorded", ErrTranscriptExhausted)
	}
	return Concluded{
		Draft:     r.transcript.Conclusion.Draft,
		Weighings: r.transcript.Conclusion.Weighings,
		Settlings: r.transcript.Conclusion.Settlings,
		Usage:     r.transcript.Usage,
	}, nil
}

// Unavailable is the model provider being down. It exists as a reasoner rather than as a flag so
// that the failure path is exercised through exactly the code a real outage goes through.
type Unavailable struct{}

func (Unavailable) Hypotheses(context.Context, Brief) (Hypothesized, error) {
	return Hypothesized{}, ErrModelUnavailable
}

func (Unavailable) Requests(context.Context, Deliberation) (Proposed, error) {
	return Proposed{}, ErrModelUnavailable
}

func (Unavailable) Conclude(context.Context, Deliberation) (Concluded, error) {
	return Concluded{}, ErrModelUnavailable
}

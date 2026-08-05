package reasoning_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// What a live round leaves behind.
//
// The property under test is not that a file appears. It is that the file a LIVE run produces is
// one the EXISTING replay accepts for the round that produced it — because the whole reason to
// record is that commit CI replays what a model actually said, and a recording the replay refuses
// is a recording of nothing.

// answering is a boundary that says something fixed. The transcripts under test are about what is
// written down, not about what a model would say, so the answers are the smallest ones that are
// still distinguishable from an empty document.
type answering struct{ pass int }

func (a *answering) Hypotheses(
	context.Context, investigation.Brief,
) (investigation.Hypothesized, error) {
	return investigation.Hypothesized{
		Hypotheses: []investigation.Hypothesis{{
			Ordinal:   1,
			Statement: "the container cannot reach its database",
			Falsifies: "a successful connection in the same window",
			State:     investigation.HypothesisLive,
		}},
		Usage: investigation.Usage{Tokens: 120, MicroCents: 40},
	}, nil
}

func (a *answering) Requests(
	_ context.Context, over investigation.Deliberation,
) (investigation.Proposed, error) {
	a.pass = over.Pass
	return investigation.Proposed{
		Weighings: []investigation.Weighing{{
			Hypothesis: 1, Evidence: 1,
			Stance: investigation.StanceSupports, Reason: "the log names the refusal",
		}},
		Usage: investigation.Usage{Tokens: 30, MicroCents: 10},
	}, nil
}

func (a *answering) Conclude(
	context.Context, investigation.Deliberation,
) (investigation.Concluded, error) {
	return investigation.Concluded{
		Draft: investigation.Draft{
			Kind:      investigation.OutcomeSupported,
			Statement: "the database refused the connection",
			Explains:  1,
			Claims: []investigation.DraftClaim{{
				Role:      investigation.ClaimSupporting,
				Statement: "the container's last output is a refused connection",
				Evidence:  []int{1},
			}},
		},
		Usage: investigation.Usage{Tokens: 900, MicroCents: 700},
	}, nil
}

func roundVersions() investigation.Versions {
	return investigation.Versions{
		Planner:       "kubernetes-workload-v1",
		Model:         "glm-5",
		PromptVersion: "4",
		SchemaVersion: "3",
		Investigator:  "test",
	}
}

func claimedRound(ordinal int) investigation.Claimed {
	return investigation.Claimed{
		Investigation: investigation.Investigation{ID: uuid.New()},
		Round: investigation.Round{
			ID:       uuid.New(),
			Ordinal:  ordinal,
			Versions: roundVersions(),
		},
	}
}

// The property the whole slice exists for.
func TestAFiledTranscriptIsOneTheExistingReplayAccepts(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	transcripts, err := reasoning.Transcripts(directory)
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}

	recording := transcripts.Begin(&answering{})
	if _, err = recording.Hypotheses(t.Context(), investigation.Brief{}); err != nil {
		t.Fatalf("hypotheses: %v", err)
	}
	if _, err = recording.Requests(
		t.Context(), investigation.Deliberation{Pass: 1}); err != nil {
		t.Fatalf("requests: %v", err)
	}
	if _, err = recording.Conclude(t.Context(), investigation.Deliberation{}); err != nil {
		t.Fatalf("conclude: %v", err)
	}

	held := claimedRound(1)
	if err = transcripts.File(
		t.Context(), held, recording.Transcript(held.Round.Versions)); err != nil {
		t.Fatalf("filing a transcript: %v", err)
	}

	written := filepath.Join(directory,
		held.Investigation.ID.String()+"-round-1.json")
	encoded, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("the recording was not filed where a reader would look: %v", err)
	}

	replay, err := investigation.LoadTranscript(encoded, held.Round.Versions)
	if err != nil {
		t.Fatalf("a live run's recording must replay for the round that produced it: %v", err)
	}
	concluded, err := replay.Conclude(t.Context(), investigation.Deliberation{})
	if err != nil {
		t.Fatalf("replaying the conclusion: %v", err)
	}
	if concluded.Draft.Kind != investigation.OutcomeSupported {
		t.Errorf("the replayed outcome is %v, want the one that was recorded", concluded.Draft.Kind)
	}
	if len(concluded.Draft.Claims) != 1 {
		t.Errorf("the replayed conclusion carries %d claims, want the 1 that was recorded",
			len(concluded.Draft.Claims))
	}
}

// A recording is keyed on the components that produced it, and the round's PINNED versions are
// what it must be keyed on. A build whose prompt moved while a round was in flight would otherwise
// file a recording claiming wording that never produced it.
func TestARecordingIsRefusedForComponentsItWasNotMadeAgainst(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	transcripts, err := reasoning.Transcripts(directory)
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}

	recording := transcripts.Begin(&answering{})
	if _, err = recording.Conclude(t.Context(), investigation.Deliberation{}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	held := claimedRound(1)
	if err = transcripts.File(
		t.Context(), held, recording.Transcript(held.Round.Versions)); err != nil {
		t.Fatalf("filing a transcript: %v", err)
	}

	encoded, err := os.ReadFile(filepath.Join(directory,
		held.Investigation.ID.String()+"-round-1.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}

	moved := roundVersions()
	moved.PromptVersion = "5"
	if _, err = investigation.LoadTranscript(encoded, moved); err == nil {
		t.Fatal("a recording made under prompt version 4 replayed under version 5; the key " +
			"exists precisely so a replayed test cannot pass against wording that no longer exists")
	}
}

// A round that FAILED is the one most worth reading, and the sweep that made this slice necessary
// is the proof: four rounds died and the record could say only that the reasoning step did not
// finish. What a recording of a failed round shows is the stage it reached.
func TestARoundThatNeverConcludedStillFilesWhatItHadSaid(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	transcripts, err := reasoning.Transcripts(directory)
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}

	recording := transcripts.Begin(&answering{})
	if _, err = recording.Hypotheses(t.Context(), investigation.Brief{}); err != nil {
		t.Fatalf("hypotheses: %v", err)
	}

	held := claimedRound(2)
	if err = transcripts.File(
		t.Context(), held, recording.Transcript(held.Round.Versions)); err != nil {
		t.Fatalf("filing a transcript for a round that did not conclude: %v", err)
	}

	encoded, err := os.ReadFile(filepath.Join(directory,
		held.Investigation.ID.String()+"-round-2.json"))
	if err != nil {
		t.Fatalf("a round that did not conclude filed nothing: %v", err)
	}

	replay, err := investigation.LoadTranscript(encoded, held.Round.Versions)
	if err != nil {
		t.Fatalf("loading the recording of a failed round: %v", err)
	}
	proposed, err := replay.Hypotheses(t.Context(), investigation.Brief{})
	if err != nil {
		t.Fatalf("replaying what it had proposed: %v", err)
	}
	if len(proposed.Hypotheses) != 1 {
		t.Errorf("the recording holds %d hypotheses, want the 1 it had formed before it died",
			len(proposed.Hypotheses))
	}
	// The stage of death is what the sweep could not answer, and it is readable here: hypotheses
	// were formed and no conclusion exists.
	if _, err = replay.Conclude(t.Context(), investigation.Deliberation{}); err == nil {
		t.Error("a recording with no conclusion must replay into the exhausted error, so a reader " +
			"can tell a round that died at the conclusion call from one that concluded")
	}
}

// Each round of a case files its own document, named so that a reinvestigated case's rounds sort
// into the order they ran in.
func TestEachRoundOfACaseFilesItsOwnRecording(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	transcripts, err := reasoning.Transcripts(directory)
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}

	first := claimedRound(1)
	second := investigation.Claimed{
		Investigation: first.Investigation,
		Round: investigation.Round{
			ID: uuid.New(), Ordinal: 2, Versions: roundVersions(),
		},
	}
	for _, held := range []investigation.Claimed{first, second} {
		recording := transcripts.Begin(&answering{})
		if _, err = recording.Conclude(t.Context(), investigation.Deliberation{}); err != nil {
			t.Fatalf("conclude: %v", err)
		}
		if err = transcripts.File(
			t.Context(), held, recording.Transcript(held.Round.Versions)); err != nil {
			t.Fatalf("filing round %d: %v", held.Round.Ordinal, err)
		}
	}

	filed, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		t.Fatalf("listing the directory: %v", err)
	}
	if len(filed) != 2 {
		t.Fatalf("two rounds filed %d recordings, want one each: %v", len(filed), filed)
	}
	for _, ordinal := range []int{1, 2} {
		name := filepath.Join(directory,
			first.Investigation.ID.String()+"-round-"+string(rune('0'+ordinal))+".json")
		if _, err = os.Stat(name); err != nil {
			t.Errorf("round %d is not filed under the name a reader would sort by: %v",
				ordinal, err)
		}
	}
}

// A recording is a case's own words. It is synthetic scenario data by intention and a customer's
// evidence by accident, and the accident is the one worth defending against.
func TestAFiledRecordingIsReadableByItsOwnerAndNobodyElse(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		// Windows does not carry POSIX mode bits, and asserting them here would assert what the
		// Go standard library synthesises rather than what the filesystem enforces.
		t.Skip("file mode is not a POSIX permission set on this platform")
	}

	directory := t.TempDir()
	transcripts, err := reasoning.Transcripts(directory)
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}
	recording := transcripts.Begin(&answering{})
	if _, err = recording.Conclude(t.Context(), investigation.Deliberation{}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	held := claimedRound(1)
	if err = transcripts.File(
		t.Context(), held, recording.Transcript(held.Round.Versions)); err != nil {
		t.Fatalf("filing a transcript: %v", err)
	}

	info, err := os.Stat(filepath.Join(directory,
		held.Investigation.ID.String()+"-round-1.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("a filed recording is mode %o, want 600", mode)
	}
}

// A directory that cannot be written to is refused at STARTUP. Discovering it at the end of a
// round means the money is already spent and the answer is already unrecoverable, which is the
// exact failure this whole change closes.
func TestADirectoryThatCannotBeWrittenToIsRefusedBeforeAnyRoundRuns(t *testing.T) {
	t.Parallel()

	if _, err := reasoning.Transcripts(""); err == nil {
		t.Error("an unnamed transcript directory was accepted")
	}

	// A path whose parent is a FILE cannot become a directory on any platform, which is what makes
	// this assertion portable where a permission bit is not.
	occupied := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
		t.Fatalf("preparing the fixture: %v", err)
	}
	if _, err := reasoning.Transcripts(filepath.Join(occupied, "recordings")); err == nil {
		t.Error("a transcript directory that cannot exist was accepted at startup")
	}
}

// The recorder must never be shared between rounds. One that were would accumulate two rounds into
// a transcript that replays as neither, and the damage would not surface until somebody replayed
// it — long after the run that produced it.
func TestEachRoundGetsARecorderOfItsOwn(t *testing.T) {
	t.Parallel()

	transcripts, err := reasoning.Transcripts(t.TempDir())
	if err != nil {
		t.Fatalf("opening a transcript directory: %v", err)
	}
	boundary := &answering{}
	first, second := transcripts.Begin(boundary), transcripts.Begin(boundary)
	if first == second {
		t.Fatal("two rounds were handed the same recorder")
	}

	if _, err = first.Conclude(t.Context(), investigation.Deliberation{}); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if second.Transcript(roundVersions()).Conclusion.Draft.Kind != 0 {
		t.Error("one round's conclusion appeared in another round's transcript")
	}
}

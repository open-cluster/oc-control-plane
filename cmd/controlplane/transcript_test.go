package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// A LIVE round leaves a recording behind.
//
// This is the wiring rather than the writing — internal/reasoning already asserts that what is
// written replays — and it is asserted here because the wiring is what was missing. The recorder
// has existed since the package was written and nothing live ever reached it: the transcript a
// deployment could be given was read only when it was REPLAYING, so a run against a real provider
// recorded nothing. The cost was paid on the first sweep, where four rounds failed and the record
// could say only that the reasoning step had not finished.

// standingInForAProvider is a boundary that is not a replay and is not the unavailable stub.
//
// The rule this asserts turns on exactly that distinction: a deployment replaying a recording
// records nothing, because it would only be re-recording the file it is replaying. Passing the
// replay itself would therefore assert the path production never takes. What a real provider is,
// from the composition root's point of view, is a boundary that is neither of those two — which is
// what this is.
type standingInForAProvider struct{ investigation.Reasoner }

func TestATranscript_IsFiledByALiveRoundWhereACorpusWouldLookForIt(t *testing.T) {
	t.Parallel()

	recordings := t.TempDir()
	plane := startInvestigationPlaneWith(t, healthyCluster(), wiring{
		reasoner:             standingInForAProvider{replaying(t, crashLoopTranscript())},
		investigatorInterval: testClaimInterval,
	}, func(cfg *config.Config) {
		cfg.ModelTranscriptDir = recordings
	})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	if summary.Investigation.Lifecycle != "concluded" {
		t.Fatalf("the case is %s, want concluded\nlogs:\n%s",
			summary.Investigation.Lifecycle, plane.logs.String())
	}

	// Named by the case and the round's ordinal, so a reinvestigated case's rounds sort into the
	// order they ran in rather than scattering under identifiers.
	filed := filepath.Join(recordings, summary.Investigation.ID+"-round-1.json")
	encoded, err := os.ReadFile(filed)
	if err != nil {
		t.Fatalf("a live round filed no recording: %v\nlogs:\n%s", err, plane.logs.String())
	}

	// The recording is keyed on the components that produced the round, which is what makes it
	// usable as a replay corpus at all: a build carrying a different prompt refuses it rather than
	// proving itself against wording that no longer exists.
	replay, err := investigation.LoadTranscript(encoded, investigatorVersions(nil))
	if err != nil {
		t.Fatalf("the recording does not replay for the build that made it: %v", err)
	}
	concluded, err := replay.Conclude(t.Context(), investigation.Deliberation{})
	if err != nil {
		t.Fatalf("replaying the recorded conclusion: %v", err)
	}
	if concluded.Draft.Kind != investigation.OutcomeSupported {
		t.Errorf("the recorded outcome is %v, want the supported one the round reached",
			concluded.Draft.Kind)
	}
}

// A deployment that is REPLAYING records nothing, and the reason is that the alternative is worse
// than the gap: a corpus holding copies of the recordings it was replaying would look like
// evidence of what a model said and be evidence of what this build echoed.
func TestATranscript_IsNotRecordedByADeploymentThatIsReplayingOne(t *testing.T) {
	t.Parallel()

	recordings := t.TempDir()
	plane := startInvestigationPlaneWith(t, healthyCluster(), wiring{
		reasoner:             replaying(t, crashLoopTranscript()),
		investigatorInterval: testClaimInterval,
	}, func(cfg *config.Config) {
		cfg.ModelTranscriptDir = recordings
	})

	opened := plane.openInvestigation(t, time.Hour)
	if summary := plane.awaitTerminal(t, opened.Investigation.ID); summary.Investigation.Lifecycle != "concluded" {
		t.Fatalf("the case is %s, want concluded\nlogs:\n%s",
			summary.Investigation.Lifecycle, plane.logs.String())
	}

	written, err := filepath.Glob(filepath.Join(recordings, "*.json"))
	if err != nil {
		t.Fatalf("listing the directory: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("a replaying deployment filed %v; re-recording a recording produces a copy of "+
			"the file being replayed", written)
	}
}

// A deployment that named no directory files nothing, which is what every deployment did before
// this existed and what an installation that did not ask for recordings keeps getting.
func TestATranscript_IsNotRecordedByADeploymentThatAskedForNone(t *testing.T) {
	t.Parallel()

	plane := startInvestigationPlaneWith(t, healthyCluster(), wiring{
		reasoner:             standingInForAProvider{replaying(t, crashLoopTranscript())},
		investigatorInterval: testClaimInterval,
	}, nil)

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	if summary.Investigation.Lifecycle != "concluded" {
		t.Fatalf("a deployment recording nothing must still investigate; the case is %s\nlogs:\n%s",
			summary.Investigation.Lifecycle, plane.logs.String())
	}
}

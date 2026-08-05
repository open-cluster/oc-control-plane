package reasoning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// WHERE A LIVE ROUND'S RECORDING GOES.
//
// The recorder above has existed since this package was written and nothing live ever used it: a
// deployment could be given a transcript to REPLAY, and a deployment answering from a real
// provider left nothing behind at all. The cost of that was paid on the first sweep, where four
// rounds failed and none of them could be read afterwards — the record could say that the
// reasoning step did not finish and nothing about what it had said before it stopped.
//
// One file per round, because a transcript is per round. The replay this feeds reads exactly one.

// transcriptFileMode keeps a recording readable by its owner and nobody else. It is synthetic
// scenario data by intention and a customer's evidence by accident, and the accident is the one
// worth defending against.
const transcriptFileMode = 0o600

// TranscriptDirectory files each round's recording as a JSON document in one directory.
//
// It satisfies the investigator's own transcript interface, so the domain never learns that a
// provider or a filesystem exists — the same relationship this package has to the boundary it
// implements.
type TranscriptDirectory struct {
	path string
}

// Transcripts files recordings under a directory, refusing one that cannot be written to.
//
// The check is here, at startup, rather than at the end of the first round. A round is the
// expensive thing in this system: discovering then that its recording has nowhere to go means the
// money is already spent and the answer is already unrecoverable.
func Transcripts(directory string) (*TranscriptDirectory, error) {
	if directory == "" {
		return nil, fmt.Errorf("a transcript directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("preparing the transcript directory: %w", err)
	}
	probe, err := os.CreateTemp(directory, ".writable-*")
	if err != nil {
		return nil, fmt.Errorf("the transcript directory is not writable: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)

	return &TranscriptDirectory{path: directory}, nil
}

// Begin wraps the boundary for one round.
func (d *TranscriptDirectory) Begin(reasoner investigation.Reasoner) investigation.Transcribed {
	return Recording(reasoner)
}

// File writes one round's recording.
//
// The name carries the investigation and the round's ordinal rather than the round's identifier,
// because a case's rounds then sort into the order they ran in — which is how somebody reads a
// case that was reinvestigated three times, and a UUID would scatter them.
func (d *TranscriptDirectory) File(
	ctx context.Context, of investigation.Claimed, transcript investigation.Transcript,
) error {
	encoded, err := json.MarshalIndent(transcript, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the transcript: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}

	final := filepath.Join(d.path, fmt.Sprintf("%s-round-%d.json",
		of.Investigation.ID, of.Round.Ordinal))

	// Written under a temporary name in the SAME directory and renamed over. A reader that
	// arrives mid-write would otherwise find a truncated document and refuse it as malformed,
	// which is a recording lost to a race rather than to anything that went wrong. Same directory
	// because a rename across filesystems is a copy and is not atomic.
	partial, err := os.CreateTemp(d.path, ".transcript-*")
	if err != nil {
		return fmt.Errorf("opening the transcript: %w", err)
	}
	written := partial.Name()
	defer func() { _ = os.Remove(written) }()

	if _, err = partial.Write(encoded); err != nil {
		_ = partial.Close()
		return fmt.Errorf("writing the transcript: %w", err)
	}
	if err = partial.Close(); err != nil {
		return fmt.Errorf("closing the transcript: %w", err)
	}
	if err = os.Chmod(written, transcriptFileMode); err != nil {
		return fmt.Errorf("securing the transcript: %w", err)
	}
	if err = os.Rename(written, final); err != nil {
		return fmt.Errorf("filing the transcript: %w", err)
	}
	return nil
}

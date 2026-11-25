package app

import (
	"context"
	"slices"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestDraftWithholdsSplitSecret(t *testing.T) {
	const secret = "draft-test-canary"
	for _, test := range []struct {
		raw, want string
	}{
		{"preview draft-test", "preview "},
		{"preview draft-test-canary continues", "preview [redacted] continues"},
		{"preview draft-test-data", "preview draft-test-data"},
		{"draft-test-canary draft-test", "[redacted] "},
	} {
		if got := safeDraftText(test.raw, secret); got != test.want {
			t.Errorf("safe draft = %q, want %q", got, test.want)
		}
	}
}

func TestNativeModelCallReplacesChatDraft(t *testing.T) {
	stream := make(chan event.Event, 8)
	stream <- event.ModelCallStartEvent{}
	stream <- event.TextBlockDeltaEvent{Delta: "Checking evidence."}
	stream <- event.ThinkingBlockDeltaEvent{Delta: "private reasoning"}
	stream <- event.ModelCallStartEvent{}
	stream <- event.TextBlockDeltaEvent{Delta: "The first check passed."}
	stream <- event.ReplyEndEvent{FinishedReason: types.ReplyCompleted}
	close(stream)

	var drafts []string
	outcome, err := drainExecution(t.Context(), stream, func(_ context.Context, text string) error {
		drafts = append(drafts, text)
		return nil
	}, "")
	if want := []string{"Checking evidence.", "", "The first check passed."}; err != nil ||
		!outcome.completed || !slices.Equal(drafts, want) {
		t.Fatalf("stream published %q, want %q, completed = %t: %v", drafts, want, outcome.completed, err)
	}
}

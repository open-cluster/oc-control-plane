package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
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
		if got := safeDraftText(test.raw, modelSecretForms(secret)); got != test.want {
			t.Errorf("safe draft = %q, want %q", got, test.want)
		}
	}
}

func TestDraftRetriesClearingAfterTransientFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	stream := make(chan event.Event, 2)
	stream <- event.TextBlockDeltaEvent{Delta: "Previous call"}
	stream <- event.ModelCallStartEvent{}
	var attempts []string
	_, err := drainExecution(ctx, stream, func(_ context.Context, text string) error {
		attempts = append(attempts, text)
		if len(attempts) == 2 {
			return errors.New("temporary database error")
		}
		if text == "" {
			close(stream)
		}
		return nil
	}, "")
	if want := []string{"Previous call", "", ""}; err != nil || !slices.Equal(attempts, want) {
		t.Fatalf("draft writes = %q, want %q: %v", attempts, want, err)
	}
}

func TestDraftClearsAfterUnacknowledgedWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	stream := make(chan event.Event, 2)
	stream <- event.TextBlockDeltaEvent{Delta: "Previous call"}
	stream <- event.ModelCallStartEvent{}
	var attempts []string
	_, err := drainExecution(ctx, stream, func(_ context.Context, text string) error {
		attempts = append(attempts, text)
		if len(attempts) == 1 {
			return errors.New("draft commit acknowledgement lost")
		}
		if text == "" {
			close(stream)
		}
		return nil
	}, "")
	if want := []string{"Previous call", ""}; err != nil || !slices.Equal(attempts, want) {
		t.Fatalf("draft writes = %q, want %q: %v", attempts, want, err)
	}
}

func TestRedactedReplyPreservesNativeContentOrder(t *testing.T) {
	thinking := message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"signature": "retained"}}
	native := message.NewMsg("agent", message.RoleAssistant, []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "read"},
		message.TextBlock{Type: "text", ID: "first", Text: "first part"},
		thinking,
		message.TextBlock{Type: "text", ID: "last", Text: "last part"},
	})
	got := replaceFinalReply(native, 1, "redacted answer")
	if got.ID != native.ID || len(got.Content) != 4 ||
		got.Content[0].(message.ToolResultBlock).ID != "read" ||
		got.Content[1] != (message.TextBlock{Type: "text", ID: "first", Text: "redacted a"}) ||
		got.Content[2].(message.ThinkingBlock).Extra["signature"] != "retained" ||
		got.Content[3] != (message.TextBlock{Type: "text", ID: "last", Text: "nswer"}) {
		t.Fatalf("the reply rewrite reordered native blocks: %#v", got.Content)
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

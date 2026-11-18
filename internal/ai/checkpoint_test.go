package ai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"

	"github.com/open-cluster/oc-control-plane/internal/ai"
)

func TestCompactionPreservesContextAcrossCheckpointRestore(t *testing.T) {
	const fact = "checkout uses port 8443"
	const oldDetail = "discardable detail "
	var summarized atomic.Bool
	provider := nativeModel(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool            `json:"stream"`
			Tools    json.RawMessage `json:"tools"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !request.Stream {
			if !bytes.Contains(request.Tools, []byte("generate_structured_output")) {
				t.Error("compaction did not request the structured summary tool")
				return
			}
			summary := "Unknown"
			if bytes.Contains(request.Messages, []byte(fact)) {
				summary = fact
				summarized.Store(true)
			}
			arguments := fmt.Sprintf(`{"task_overview":"Checkout","current_state":%q,"important_discoveries":"None","next_steps":"Continue","context_to_preserve":%q}`, summary, summary)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"summary","type":"function","function":{"name":"generate_structured_output","arguments":%q}}]},"finish_reason":"tool_calls"}]}`, arguments)
			return
		}
		answer := "recorded"
		if bytes.Contains(request.Messages, []byte("Which port?")) {
			if !bytes.Contains(request.Messages, []byte(fact)) || bytes.Contains(request.Messages, []byte(oldDetail)) {
				t.Error("restored request lacks the retained fact or includes the compacted detail")
			}
			answer = "8443"
		}
		writeChunks(w, fmt.Sprintf(`{"content":%q}`, answer))
		finishStream(w, "stop")
	})
	saver := &checkpointSaver{}
	config := ai.Config{
		Name:            "fixture",
		SessionID:       "compaction",
		Model:           provider,
		StateSaver:      saver,
		Context:         agent.ContextConfig{ContextSize: 320},
		ToolConcurrency: 1,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	runtime := newAgent(t, config, nil)
	for _, input := range []string{
		fact + " " + strings.Repeat(oldDetail, 30),
		strings.Repeat("Recent diagnostic data. ", 24),
		"Keep the important setting for the next question.",
	} {
		stream, err := runtime.ReplyStream(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if text, end := readReply(stream); text != "recorded" || end.FinishedReason != types.ReplyCompleted {
			t.Fatalf("expected completed turn, got %q, %+v", text, end)
		}
	}
	checkpoint, err := agent.LoadCheckpoint(ctx, saver, config.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !summarized.Load() || !strings.Contains(checkpoint.Summary, fact) {
		t.Fatal("automatic structured summary did not retain the fact in the checkpoint")
	}
	restored := newAgent(t, config, checkpoint)
	stream, err := restored.ReplyStream(ctx, "Which port?")
	if err != nil {
		t.Fatal(err)
	}
	if text, end := readReply(stream); text != "8443" || end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("restored reply = %q, %+v", text, end)
	}
}

func TestCheckpointRecovery(t *testing.T) {
	t.Run("resume interrupted reply", func(t *testing.T) {
		const input = "one accepted request"
		var requests atomic.Int32
		provider := nativeModel(t, func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Messages json.RawMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if bytes.Count(request.Messages, []byte(input)) != 1 {
				t.Error("accepted input was lost or repeated")
			}
			if requests.Add(1) == 1 {
				writeChunks(w, `{"content":"partial"}`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			writeChunks(w, `{"content":"resumed"}`)
			finishStream(w, "stop")
		})
		saver := &checkpointSaver{}
		config := ai.Config{
			Name:            "fixture",
			SessionID:       "recovery",
			Model:           provider,
			StateSaver:      saver,
			ToolConcurrency: 1,
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		interrupted, interrupt := context.WithCancel(ctx)
		defer interrupt()
		runtime := newAgent(t, config, nil)
		stream, err := runtime.ReplyStream(interrupted, input)
		if err != nil {
			t.Fatal(err)
		}
		var replyID string
		for item := range stream {
			if delta, ok := item.(event.TextBlockDeltaEvent); ok && delta.Delta == "partial" {
				replyID = delta.GetReplyID()
				interrupt()
			}
		}
		checkpoint, err := agent.LoadCheckpoint(ctx, saver, config.SessionID)
		if err != nil || replyID == "" || checkpoint.ReplyID != replyID {
			t.Fatalf("interrupted reply identity %q was not saved: %v", replyID, err)
		}
		restored := newAgent(t, config, checkpoint)
		stream, err = restored.ResumeReplyStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if text, end := readReply(stream); text != "resumed" || end.ReplyID != replyID || end.FinishedReason != types.ReplyCompleted {
			t.Fatalf("resumed reply = %q, %+v", text, end)
		}
		if _, err := restored.ResumeReplyStream(ctx); err == nil {
			t.Fatal("completed reply could be resumed")
		}
	})

	t.Run("reject invalid state and surface save failure", func(t *testing.T) {
		provider := nativeModel(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("called provider after checkpoint failure")
			w.WriteHeader(http.StatusInternalServerError)
		})
		config := ai.Config{
			Name:            "fixture",
			SessionID:       "recovery",
			Model:           provider,
			ToolConcurrency: 1,
		}
		for _, payload := range []string{"", "{", `{"session_id":"recovery","schema_version":999}`} {
			saver := &checkpointSaver{payload: []byte(payload)}
			if _, err := agent.LoadCheckpoint(t.Context(), saver, config.SessionID); err == nil {
				t.Fatalf("loaded missing, corrupt or unsupported checkpoint %q", payload)
			}
		}
		config.StateSaver = &checkpointSaver{}
		if _, err := ai.NewAgent(config, &agent.AgentState{SessionID: "foreign", SchemaVersion: 1}); err == nil {
			t.Fatal("accepted a checkpoint from a different session")
		}
		config.Permissions = permission.NewContext(permission.ModeDefault)
		checkpoint := &agent.AgentState{
			SessionID:     config.SessionID,
			SchemaVersion: 1,
			PermissionCtx: &permission.Context{WorkingDirectories: map[string]permission.WorkingDirectory{
				"/retired": {Path: "/retired"},
			}},
		}
		restored := newAgent(t, config, checkpoint)
		if err := restored.SaveCheckpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		saved, err := agent.LoadCheckpoint(t.Context(), config.StateSaver, config.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := saved.PermissionCtx.WorkingDirectories["/retired"]; exists {
			t.Fatal("checkpoint restored a retired permission grant")
		}
		config.StateSaver = &checkpointSaver{saveErr: errors.New("fixture save failed")}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		runtime := newAgent(t, config, nil)
		stream, err := runtime.ReplyStream(ctx, "question")
		if err != nil {
			t.Fatal(err)
		}
		if _, end := readReply(stream); end.FinishedReason != types.ReplyError ||
			end.Error == nil || !strings.Contains(end.Error.Message, "checkpoint failed") {
			t.Fatalf("expected checkpoint failure, got %+v", end)
		}
	})
}

func readReply(stream <-chan event.Event) (string, event.ReplyEndEvent) {
	var text strings.Builder
	var end event.ReplyEndEvent
	for item := range stream {
		switch item := item.(type) {
		case event.TextBlockDeltaEvent:
			text.WriteString(item.Delta)
		case event.ReplyEndEvent:
			end = item
		}
	}
	return text.String(), end
}

// checkpointSaver retains a serialized snapshot rather than a shared state pointer.
type checkpointSaver struct {
	mu      sync.Mutex
	payload []byte
	saveErr error
}

func (s *checkpointSaver) SaveState(ctx context.Context, _ string, state *agent.AgentState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload = payload
	return nil
}

func (s *checkpointSaver) LoadState(_ context.Context, _ string) (*agent.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.payload) == 0 {
		return nil, nil
	}
	var state agent.AgentState
	if err := json.Unmarshal(s.payload, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (*checkpointSaver) ListSessions(context.Context) ([]agent.SessionInfo, error) {
	return nil, errors.ErrUnsupported
}

func (*checkpointSaver) DeleteSession(context.Context, string) error { return errors.ErrUnsupported }

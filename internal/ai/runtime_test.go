package ai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"

	"github.com/open-cluster/oc-control-plane/internal/ai"
)

func TestReplyStreamsBeforeProviderCompletionWithHooks(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		name := "complete"
		if truncated {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var requests, calls atomic.Int32
			provider := nativeModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Stream {
					t.Errorf("expected native streaming request, got %+v, %v", body, err)
					return
				}
				writeChunks(w, `{"content":"first "}`)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if truncated {
					writeChunks(w, `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"inspect","arguments":"{"}}]}`)
					return
				}
				writeChunks(w, `{"content":"second"}`)
				finishStream(w, "stop")
			})
			inspect := tool.NewFunctionTool("inspect", "Read a fixture", objectSchema,
				func(context.Context, map[string]any) (any, error) {
					calls.Add(1)
					return "unexpected", nil
				})
			instruments := metrics.NewInMemoryProvider()
			runtime := newAgent(t, ai.Config{
				Name:            "fixture",
				Instructions:    "Answer the question.",
				SessionID:       "session",
				Model:           provider,
				Toolkit:         tool.NewToolkit(inspect),
				ToolConcurrency: 2,
				Hooks:           []loop.Hook{metrics.NewMetricsHook(instruments)},
			}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			stream, err := runtime.ReplyStream(ctx, "question")
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			var end event.ReplyEndEvent
			for item := range stream {
				switch item := item.(type) {
				case event.TextBlockDeltaEvent:
					if text.Len() == 0 {
						if item.Delta != "first " {
							t.Errorf("first delta = %q", item.Delta)
						}
						close(release)
					}
					text.WriteString(item.Delta)
				case event.ReplyEndEvent:
					end = item
				}
			}
			if ctx.Err() != nil || end.SessionID != "session" || requests.Load() != 1 || calls.Load() != 0 {
				t.Fatalf("reply end = %+v, requests = %d, tools = %d, context = %v", end, requests.Load(), calls.Load(), ctx.Err())
			}
			if got := instruments.GetCounter(metrics.ModelCallTotal).Value(); got != 1 {
				t.Fatalf("loop hook counted %v model calls", got)
			}
			if truncated {
				if end.FinishedReason != types.ReplyError || end.Error == nil || text.String() != "first " {
					t.Fatalf("truncated reply = %q, %+v", text.String(), end)
				}
			} else if end.FinishedReason != types.ReplyCompleted || text.String() != "first second" {
				t.Fatalf("completed reply = %q, %+v", text.String(), end)
			}
		})
	}
}

var objectSchema = json.RawMessage(`{"type":"object"}`)

func nativeModel(t *testing.T, handler http.HandlerFunc) model.ChatModel {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := model.NewOpenAIChatModel(model.OpenAIConfig{
		Model:   "fixture",
		APIKey:  "fixture",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func writeChunks(w http.ResponseWriter, deltas ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, delta := range deltas {
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":%s}]}\n\n", delta)
	}
}

func finishStream(w http.ResponseWriter, reason string) {
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", reason)
}

func newAgent(t *testing.T, config ai.Config, state *agent.AgentState) *agent.UnifiedAgent {
	t.Helper()
	runtime, err := ai.NewAgent(config, state)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

package ai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"

	"github.com/open-cluster/oc-control-plane/internal/ai"
)

func TestReplyUsesBoundedToolkitAndRespectsBarriers(t *testing.T) {
	var active, finished, peak atomic.Int32
	var unsafeFinished, externalAnswered atomic.Bool
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	read := tool.NewFunctionTool("read", "Read a fixture", objectSchema,
		func(ctx context.Context, _ map[string]any) (any, error) {
			running := active.Add(1)
			for previous := peak.Load(); running > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, running) {
					break
				}
			}
			started <- struct{}{}
			defer active.Add(-1)
			select {
			case <-release:
				finished.Add(1)
				return "read", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	read.ReadOnly, read.ConcurrencySafe = true, true
	unsafe := tool.NewFunctionTool("serial", "Run an isolated fixture", objectSchema,
		func(context.Context, map[string]any) (any, error) {
			if active.Load() != 0 || finished.Load() != 3 {
				t.Error("serial tool ran before preceding reads finished")
			}
			unsafeFinished.Store(true)
			return "serial", nil
		})
	external := tool.NewFunctionTool("external", "Wait for the host fixture", objectSchema,
		func(context.Context, map[string]any) (any, error) {
			t.Error("external tool ran locally")
			return nil, nil
		})
	external.ReadOnly, external.ConcurrencySafe = true, true
	after := tool.NewFunctionTool("after", "Read after external input", objectSchema,
		func(context.Context, map[string]any) (any, error) {
			if !externalAnswered.Load() {
				t.Error("read crossed the external tool barrier")
			}
			return "after", nil
		})
	after.ReadOnly, after.ConcurrencySafe = true, true

	provider := nativeModel(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(request.Messages), "host result") {
			writeChunks(w, `{"content":"done"}`)
			finishStream(w, "stop")
			return
		}
		for index, name := range []string{"read", "read", "read", "serial", "external", "after"} {
			writeChunks(w, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"call-%d","type":"function","function":{"name":%q,"arguments":"{"}}]}`, index, index, name))
			writeChunks(w, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":"}"}}]}`, index))
		}
		finishStream(w, "tool_calls")
	})
	var admitted int
	hook := toolAdmissionHook{
		Hook: metrics.NewMetricsHook(metrics.NewInMemoryProvider()),
		before: func(name string) {
			if name == "read" {
				admitted++
				if admitted == 3 && finished.Load() == 0 {
					t.Error("third read was admitted before a slot was released")
				}
			}
		},
	}
	runtime := newAgent(t, ai.Config{
		Name:            "fixture",
		SessionID:       "tools",
		Model:           provider,
		Toolkit:         tool.NewToolkit(read, unsafe, externalTool{external}, after),
		Permissions:     permission.NewContext(permission.ModeBypass),
		ToolConcurrency: 2,
		Hooks:           []loop.Hook{hook},
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	stream, err := runtime.ReplyStream(ctx, "read")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent reads did not overlap")
		}
	}
	close(release)
	var end event.ReplyEndEvent
	for item := range stream {
		switch item := item.(type) {
		case event.RequireExternalExecutionEvent:
			if !unsafeFinished.Load() || len(item.ToolCalls) != 1 {
				t.Errorf("unexpected external handoff: %+v", item)
				continue
			}
			externalAnswered.Store(true)
			answer := event.NewExternalExecutionResultEvent(item.ReplyID, []message.ToolResultBlock{{
				ID:     item.ToolCalls[0].ID,
				State:  message.ToolResultSuccess,
				Output: "host result",
			}})
			if err := runtime.SubmitExternalResultContext(ctx, &answer); err != nil {
				t.Error(err)
			}
		case event.ReplyEndEvent:
			end = item
		}
	}
	if peak.Load() != 2 || finished.Load() != 3 || !externalAnswered.Load() || end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("peak = %d, reads = %d, external handoff = %t, end = %+v", peak.Load(), finished.Load(), externalAnswered.Load(), end)
	}
}

type externalTool struct {
	*tool.FunctionTool
}

func (externalTool) IsExternalTool() bool { return true }

type toolAdmissionHook struct {
	loop.Hook
	before func(string)
}

func (h toolAdmissionHook) BeforeToolExec(state protocol.LoopState, iteration int, name string) {
	h.before(name)
	h.Hook.BeforeToolExec(state, iteration, name)
}

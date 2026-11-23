package ai_test

import (
	"bytes"
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
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"

	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestInvestigationStructuredReportProviderWire(t *testing.T) {
	for _, provider := range []string{"anthropic", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream   bool
					Messages []struct{ Role string }
					Tools    []json.RawMessage
					Provider struct {
						ZDR               bool
						AllowFallbacks    *bool  `json:"allow_fallbacks"`
						RequireParameters bool   `json:"require_parameters"`
						DataCollection    string `json:"data_collection"`
						Sort              string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Stream || len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != "user" ||
					len(request.Tools) != 1 || !bytes.Contains(request.Tools[0], []byte("generate_structured_output")) ||
					!bytes.Contains(request.Tools[0], []byte("toolRunId")) {
					t.Error("expected a buffered request with the Conclusion schema and a trailing user message")
				}
				if len(request.Tools) == 1 && bytes.Contains(request.Tools[0], []byte("answer_only")) {
					t.Error("the report schema offered a historical-only outcome")
				}
				if provider == "openrouter" {
					policy := request.Provider
					if !policy.ZDR || policy.AllowFallbacks == nil || *policy.AllowFallbacks ||
						!policy.RequireParameters || policy.DataCollection != "deny" || policy.Sort != "price" {
						t.Error("OpenRouter report omitted its deployment policy")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":400,"message":"tool_choice is not supported while thinking (provider-canary)"}}`)
					return
				}
				if provider == "anthropic" {
					_, _ = fmt.Fprint(w, `{"id":"answer","role":"assistant","content":[{"type":"tool_use","id":"report","name":"generate_structured_output","input":{"status":"inconclusive","summary":"Cause remains unknown."}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":6}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"id":"answer","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"report","type":"function","function":{"name":"generate_structured_output","arguments":"{\"status\":\"inconclusive\",\"summary\":\"Cause remains unknown.\"}"}}]}}]}`)
			}))
			defer server.Close()

			runtime := agent.NewUnifiedAgent("investigator", "Investigate", fixtureModel(t, provider, server.URL),
				agent.WithState(&agent.AgentState{Context: []*message.Msg{
					message.UserMsg("operator", "What happened?"),
					message.AssistantMsg("investigator", []message.ContentBlock{message.TextBlock{Type: "text", Text: "No cause established."}}),
				}}))
			if err := runtime.Observe(t.Context(), []*message.Msg{message.UserMsg("operator", ai.ConclusionInstructions)}); err != nil {
				t.Fatal(err)
			}
			report, err := runtime.GenerateStructuredResponseWithUsage(t.Context(), investigation.ConclusionSchema())
			if err != nil {
				t.Fatalf("structured generation failed after %d calls: %v", calls.Load(), err)
			}
			conclusion, err := investigation.ParseConclusion(report.Output)
			if err != nil || conclusion.Summary != "Cause remains unknown." || calls.Load() != 2 {
				t.Fatalf("got report %q after %d calls, error=%v", report.Output, calls.Load(), err)
			}
		})
	}
}

func TestOpenRouterPolicyAndContinuation(t *testing.T) {
	var requests atomic.Int32
	textReceived := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream        bool
			StreamOptions json.RawMessage `json:"stream_options"`
			Provider      struct {
				ZDR               bool
				AllowFallbacks    *bool  `json:"allow_fallbacks"`
				RequireParameters bool   `json:"require_parameters"`
				DataCollection    string `json:"data_collection"`
				Sort              string
			}
			Messages       json.RawMessage
			Tools          json.RawMessage
			ToolChoice     json.RawMessage `json:"tool_choice"`
			ResponseFormat json.RawMessage `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		policy := request.Provider
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" ||
			!policy.ZDR || policy.AllowFallbacks == nil || *policy.AllowFallbacks || !policy.RequireParameters ||
			policy.DataCollection != "deny" || policy.Sort != "price" {
			t.Errorf("unexpected OpenRouter route or policy: %s %+v", r.URL.Path, policy)
		}
		if len(request.StreamOptions) != 0 {
			t.Error("OpenRouter request sent obsolete stream_options")
		}

		number := requests.Add(1)
		if request.Stream != (number == 1) {
			t.Errorf("request %d has stream=%t", number, request.Stream)
		}
		if number == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			writeOpenRouterFrame(w, `[{"index":0,"delta":{"content":"Checking."}}]`)
			w.(http.Flusher).Flush()
			select {
			case <-textReceived:
			case <-r.Context().Done():
				return
			}
			writeOpenRouterFrame(w, `[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"encrypted-fixture","format":"unknown","index":0,"id":"detail-e"},{"type":"reasoning.text","text":"private","signature":"signed-fixture","format":"anthropic-claude-v1","index":1,"id":"detail-t"}],"tool_calls":[{"index":0,"id":"lookup-1","type":"function","function":{"name":"lookup","arguments":"{\"port\":"}}]}}]`)
			writeOpenRouterFrame(w, `[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"index":0,"function":{"arguments":"8443}"}}]}}]`)
			_, _ = fmt.Fprint(w, `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[],"usage":{"prompt_tokens":18,"completion_tokens":3,"total_tokens":21,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2}}}`+"\n\n")
			_, _ = fmt.Fprint(w, "id: final\r\ndata:[DONE]\r\n\r\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if number == 2 {
			for _, marker := range []string{`"reasoning_details"`, `"signed-fixture"`, `"encrypted-fixture"`, `"tool_call_id":"lookup-1"`} {
				if !bytes.Contains(request.Messages, []byte(marker)) {
					t.Errorf("continuation omitted %s", marker)
				}
			}
		}
		if number == 3 {
			if string(request.ToolChoice) != `"required"` ||
				!bytes.Contains(request.Tools, []byte("generate_structured_output")) {
				t.Error("compaction's native structured Tool is missing")
			}
			_, _ = fmt.Fprint(w, `{"id":"summary","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"summary-tool","type":"function","function":{"name":"generate_structured_output","arguments":"{\"summary\":\"checkout\"}"}}]}}]}`)
			return
		}
		if number == 4 && !bytes.Contains(request.ResponseFormat, []byte(`"json_schema"`)) {
			t.Error("structured request omitted its response format")
		}
		_, _ = fmt.Fprint(w, `{"id":"answer","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\":\"checkout\"}"}}]}`)
	}))
	defer provider.Close()

	chat := fixtureModel(t, "openrouter", provider.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	input := []*message.Msg{message.UserMsg("operator", "Which port?")}
	stream, err := chat.ChatStream(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var final model.ChatResponse
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatal(chunk.Error)
		}
		if !chunk.IsLast && chunk.GetTextContent() == "Checking." {
			close(textReceived)
		}
		if chunk.IsLast {
			final = chunk
		}
	}
	if final.StopReason != model.StopReasonToolCalls || final.Usage == nil ||
		final.Usage.InputTokens != 12 || final.Usage.OutputTokens != 3 ||
		final.Usage.TotalInputTokens != 18 || final.Usage.CacheInputTokens != 4 || final.Usage.CacheCreationInputTokens != 2 {
		t.Fatalf("unexpected terminal response or usage: %+v", final)
	}
	var call message.ToolCallBlock
	for _, block := range final.Content {
		if toolCall, ok := block.(message.ToolCallBlock); ok {
			call = toolCall
		}
	}
	if call.ID != "lookup-1" || call.Input != `{"port":8443}` {
		t.Fatalf("fragmented Tool call did not assemble: %+v", call)
	}
	state, err := json.Marshal(agent.AgentState{
		SessionID: "fixture",
		Context:   append(input, message.AssistantMsg("agent", final.Content)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var restored agent.AgentState
	if err := json.Unmarshal(state, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Context = append(restored.Context, &message.Msg{
		Role: message.RoleUser,
		Content: []message.ContentBlock{message.ToolResultBlock{
			Type:   "tool_result",
			ID:     call.ID,
			Name:   call.Name,
			Output: "8443",
		}},
	})
	if reply, err := chat.Chat(ctx, restored.Context); err != nil || reply.GetTextContent() != `{"summary":"checkout"}` {
		t.Fatalf("restored continuation failed: %v", err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	if summary, _, err := model.GenerateStructuredOutputWithUsage(ctx, chat, input, schema); err != nil ||
		string(summary) != `{"summary":"checkout"}` {
		t.Fatalf("compaction request failed: %q, %v", summary, err)
	}
	report, err := chat.Chat(ctx, input, model.WithResponseFormat(&model.ResponseFormat{
		Type:       "json_schema",
		JSONSchema: schema,
		Name:       "report",
		Strict:     true,
	}))
	if err != nil || report.GetTextContent() != `{"summary":"checkout"}` || requests.Load() != 4 {
		t.Fatalf("structured request failed: %v, requests=%d", err, requests.Load())
	}
}

func TestOpenRouterRejectsIncompleteReplies(t *testing.T) {
	const canary = "private-provider-error"
	for _, test := range []struct {
		name    string
		trailer string
	}{
		{
			name:    "EOF after finish without DONE",
			trailer: `{"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		},
		{
			name:    "provider error",
			trailer: `{"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[],"error":{"code":502,"message":"` + canary + `"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeOpenRouterFrame(w, `[{"index":0,"delta":{"content":"Checking.","tool_calls":[{"index":0,"id":"lookup-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]`)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", test.trailer)
			}))
			defer provider.Close()
			runtime, err := ai.NewAgent(ai.Config{
				Name:            "fixture",
				SessionID:       "terminal",
				Model:           fixtureModel(t, "openrouter", provider.URL),
				ToolConcurrency: 1,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			events, err := runtime.ReplyStream(t.Context(), "Check the port")
			if err != nil {
				t.Fatal(err)
			}
			var end event.ReplyEndEvent
			for item := range events {
				switch item := item.(type) {
				case event.ToolCallStartEvent:
					t.Error("incomplete provider stream published a Tool call")
				case event.ReplyEndEvent:
					end = item
				}
			}
			if requests.Load() != 1 || end.FinishedReason != types.ReplyError || end.Error == nil ||
				strings.Contains(end.Error.Message, canary) {
				t.Fatalf("expected one redacted terminal failure, got %d requests, %+v", requests.Load(), end)
			}
		})
	}
}

func TestProviderRetryCounts(t *testing.T) {
	for _, test := range []struct {
		provider string
		buffered int32
	}{
		{provider: "anthropic", buffered: 3},
		{provider: "openai", buffered: 3},
		{provider: "openrouter", buffered: 1},
	} {
		t.Run(test.provider, func(t *testing.T) {
			var buffered, streaming atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Stream bool }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Stream {
					streaming.Add(1)
				} else {
					buffered.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited"}}`)
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			chat := fixtureModel(t, test.provider, provider.URL)
			input := []*message.Msg{message.UserMsg("operator", "check")}
			_, err := chat.Chat(ctx, input, model.WithRetries(2, time.Millisecond))
			if !model.IsRetryableError(err) || buffered.Load() != test.buffered || streaming.Load() != 0 {
				t.Fatalf("buffered Chat made %d requests, want %d, error=%v", buffered.Load(), test.buffered, err)
			}
			runtime, err := ai.NewAgent(ai.Config{
				Name:            "fixture",
				SessionID:       "retry",
				Model:           chat,
				ToolConcurrency: 1,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			events, err := runtime.ReplyStream(ctx, "check")
			if err != nil {
				t.Fatal(err)
			}
			var end event.ReplyEndEvent
			for item := range events {
				if terminal, ok := item.(event.ReplyEndEvent); ok {
					end = terminal
				}
			}
			if streaming.Load() != 3 || buffered.Load() != test.buffered || end.FinishedReason != types.ReplyError {
				t.Fatalf("UnifiedAgent made %d streamed and %d additional buffered requests, end=%+v",
					streaming.Load(), buffered.Load()-test.buffered, end)
			}
		})
	}
}

func fixtureModel(t *testing.T, provider, endpoint string) model.ChatModel {
	t.Helper()
	chat, err := ai.NewChatModel(config.Config{
		ModelProvider: provider,
		ModelName:     "fixture",
		ModelAPIKey:   "fixture-key",
	}, endpoint, 0)
	if err != nil {
		t.Fatal(err)
	}
	return chat
}

func writeOpenRouterFrame(w http.ResponseWriter, choices string) {
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"reply\",\"model\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":%s}\n\n", choices)
}

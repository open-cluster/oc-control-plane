package ai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	aserr "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestNativeAnthropicModel(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "fixture-key" ||
			request.Model != "claude-fixture" || request.MaxTokens != 1400 || !request.Stream {
			t.Errorf("unexpected Anthropic request to %s: %+v", r.URL.Path, request)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for eventIndex, event := range []string{
			`{"type":"message_start","message":{"id":"fixture","model":"claude-fixture","usage":{"input_tokens":14}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checkout healthy."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", []string{"message_start", "content_block_start",
				"content_block_delta", "content_block_stop", "message_delta", "message_stop"}[eventIndex], event)
		}
	}))
	defer provider.Close()

	chat, err := ai.NewChatModel(config.Config{
		ModelProvider:        "anthropic",
		ModelName:            "claude-fixture",
		ModelAPIKey:          "fixture-key",
		ModelMaxOutputTokens: 1400,
	}, provider.URL, 64_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := chat.(model.ChatModelUnwrapper).Unwrap().(*model.AnthropicChatModel); !ok {
		t.Fatalf("got %T, want the native Anthropic ChatModel", chat)
	}
	stream, err := chat.ChatStream(t.Context(), []*message.Msg{message.UserMsg("operator", "How is checkout?")})
	if err != nil {
		t.Fatal(err)
	}
	var sawDelta bool
	var final model.ChatResponse
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatal(chunk.Error)
		}
		sawDelta = sawDelta || !chunk.IsLast && chunk.GetTextContent() == "Checkout healthy."
		if chunk.IsLast {
			final = chunk
		}
	}
	if !sawDelta || final.GetTextContent() != "Checkout healthy." ||
		final.StopReason != model.StopReasonStop || final.Usage == nil || final.Usage.InputTokens != 14 || final.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected native stream completion: %+v", final)
	}
}

func TestOpenRouterContinuation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	textReceived := make(chan struct{})
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		var request struct {
			Stream   bool
			Provider struct {
				ZDR               bool
				AllowFallbacks    *bool  `json:"allow_fallbacks"`
				RequireParameters bool   `json:"require_parameters"`
				DataCollection    string `json:"data_collection"`
				Sort              string
			}
			Messages       json.RawMessage
			ToolChoice     json.RawMessage `json:"tool_choice"`
			Tools          json.RawMessage
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
			return
		}
		if number >= 3 {
			w.Header().Set("Content-Type", "application/json")
			switch number {
			case 3:
				var frames []struct {
					Role      string
					Reasoning json.RawMessage       `json:"reasoning_details"`
					ToolCalls []struct{ ID string } `json:"tool_calls"`
				}
				if err := json.Unmarshal(request.Messages, &frames); err != nil {
					t.Error(err)
				}
				var rounds int
				for _, frame := range frames {
					if frame.Role != "assistant" || len(frame.ToolCalls) == 0 {
						continue
					}
					rounds++
					switch frame.ToolCalls[0].ID {
					case "lookup-1":
						if !bytes.Contains(frame.Reasoning, []byte("encrypted-fixture")) ||
							!bytes.Contains(frame.Reasoning, []byte("signed-fixture")) ||
							bytes.Contains(frame.Reasoning, []byte("signature-round-two")) {
							t.Error("first assistant frame lost its own reasoning")
						}
					case "lookup-2":
						if !bytes.Contains(frame.Reasoning, []byte("signature-round-two")) ||
							bytes.Contains(frame.Reasoning, []byte("signed-fixture")) {
							t.Error("second assistant frame lost its own reasoning")
						}
					default:
						t.Error("unexpected historical tool call")
					}
				}
				if rounds != 2 {
					t.Errorf("got %d assistant tool-call frames, want two", rounds)
				}
				_, _ = fmt.Fprint(w, `{"id":"answer","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"8443."}}],"usage":{"prompt_tokens":18}}`)
			case 4:
				if string(request.ToolChoice) != `"required"` ||
					!bytes.Contains(request.Tools, []byte("generate_structured_output")) {
					t.Error("the native structured helper lost its forced Tool")
				}
				_, _ = fmt.Fprint(w, `{"id":"summary","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"summary-tool","type":"function","function":{"name":"generate_structured_output","arguments":"{\"summary\":\"checkout\"}"}}]}}]}`)
			case 5:
				for _, field := range []string{`"json_schema"`, `"strict":true`, `"name":"report"`} {
					if !bytes.Contains(request.ResponseFormat, []byte(field)) {
						t.Errorf("structured response format omitted %s", field)
					}
				}
				_, _ = fmt.Fprint(w, `{"id":"report","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\":\"checkout\"}"}}]}`)
			}
			return
		}
		if number == 2 {
			for _, marker := range []string{`"reasoning_details"`, `"reasoning.encrypted"`, `"signed-fixture"`, `"encrypted-fixture"`, `"tool_call_id":"lookup-1"`} {
				if !bytes.Contains(request.Messages, []byte(marker)) {
					t.Errorf("continuation omitted %s", marker)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"follow-up","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"lookup-2","type":"function","function":{"name":"lookup","arguments":"{}"}}],"reasoning_details":[{"type":"reasoning.text","text":"private","signature":"signature-round-two","format":"anthropic-claude-v1","index":0,"id":"round-2"}]}}],"usage":{"prompt_tokens":18,"completion_tokens":3,"total_tokens":21}}`)
			return
		}
		if !request.Stream {
			t.Error("OpenRouter stream request was buffered")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":null,"delta":{"content":"Checking."}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-textReceived:
		case <-r.Context().Done():
			return
		}
		for _, frame := range []string{
			`{"index":0,"finish_reason":null,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"encrypted-fixture","format":"unknown","index":0,"id":"detail-e"},{"type":"reasoning.text","text":"private","signature":"signed-fixture","format":"anthropic-claude-v1","index":1,"id":"detail-t"}],"tool_calls":[{"index":0,"id":"lookup-1","type":"function","function":{"name":"lookup","arguments":"{\"port\":"}}]}}`,
			`{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"8443}"}}]}}`,
			`{"index":0,"finish_reason":"tool_calls","delta":{}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"reply\",\"model\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[%s]}\n\n", frame)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n: keepalive\n\n")
	}))
	defer provider.Close()

	chat, err := ai.NewChatModel(config.Config{ModelProvider: "openrouter", ModelName: "fixture", ModelAPIKey: "fixture-key"}, provider.URL, 64_000)
	if err != nil {
		t.Fatal(err)
	}
	input := []*message.Msg{message.UserMsg("operator", "Which port?")}
	stream, err := chat.ChatStream(ctx, input, model.WithTools([]model.ToolSchema{{
		Type:     "function",
		Function: model.ToolFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}))
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
	var call message.ToolCallBlock
	for _, block := range final.Content {
		if toolCall, ok := block.(message.ToolCallBlock); ok {
			call = toolCall
		}
	}
	if final.StopReason != model.StopReasonToolCalls || call.ID != "lookup-1" || call.Input != `{"port":8443}` {
		t.Fatalf("tool call did not assemble: %+v", call)
	}
	stateJSON, err := json.Marshal(agent.AgentState{SessionID: "fixture", Context: append(input, message.AssistantMsg("agent", final.Content))})
	if err != nil {
		t.Fatal(err)
	}
	var restored agent.AgentState
	if err := json.Unmarshal(stateJSON, &restored); err != nil {
		t.Fatal(err)
	}
	toolReply := &message.Msg{
		Role: message.RoleUser,
		Content: []message.ContentBlock{message.ToolResultBlock{
			Type: "tool_result", ID: call.ID, Name: call.Name, Output: "8443",
		}},
	}
	response, err := chat.Chat(ctx, append(restored.Context, toolReply))
	if err != nil || response.StopReason != model.StopReasonToolCalls || response.Usage == nil || response.Usage.InputTokens != 18 {
		t.Fatalf("restored continuation failed: %v", err)
	}
	// AgentScope retains consecutive tool rounds in one assistant message.
	merged := slices.Clone(restored.Context[1].Content)
	merged = append(merged, toolReply.Content...)
	merged = append(merged, response.Content...)
	merged = append(merged, message.ToolResultBlock{Type: "tool_result", ID: "lookup-2", Name: "lookup", Output: "8443"})
	stateJSON, err = json.Marshal(agent.AgentState{SessionID: "fixture", Context: append(input, message.AssistantMsg("agent", merged))})
	if err != nil || json.Unmarshal(stateJSON, &restored) != nil {
		t.Fatal("could not round-trip merged tool rounds")
	}
	answer, err := chat.Chat(ctx, restored.Context)
	if err != nil || answer.GetTextContent() != "8443." || answer.Usage != nil {
		t.Fatalf("merged continuation failed: %v", err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`)
	summary, _, err := model.GenerateStructuredOutputWithUsage(ctx, chat, input, schema)
	if err != nil || !bytes.Contains(summary, []byte("checkout")) {
		t.Fatalf("native compaction-style structured request failed: %v", err)
	}
	report, err := chat.Chat(ctx, input, model.WithResponseFormat(&model.ResponseFormat{
		Type: "json_schema", JSONSchema: schema, Name: "report", Strict: true,
	}))
	if err != nil || report.GetTextContent() != `{"summary":"checkout"}` {
		t.Fatalf("strict structured request failed: %v", err)
	}
}

func TestNativeProviderErrorsAreRedacted(t *testing.T) {
	const canary = "fixture-provider-response-do-not-expose"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !request.Stream {
			http.Error(w, canary, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":%q}}\n\n", canary)
	}))
	defer server.Close()
	chat, err := ai.NewChatModel(config.Config{ModelProvider: "anthropic", ModelName: "fixture", ModelAPIKey: "fixture-key"}, server.URL, 64_000)
	if err != nil {
		t.Fatal(err)
	}
	input := []*message.Msg{message.UserMsg("operator", "hello")}
	if _, err := chat.Chat(t.Context(), input); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatal("buffered provider failure was missing or carried a raw provider payload")
	}
	stream, err := chat.ChatStream(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for chunk := range stream {
		if chunk.Error == nil {
			continue
		}
		failed = true
		if strings.Contains(chunk.Error.Error(), canary) {
			t.Fatal("stream error carried a raw provider payload")
		}
	}
	if !failed {
		t.Fatal("provider stream error was not terminal")
	}
}

func TestOllamaContextMatchesRequestCapacity(t *testing.T) {
	const capacity = 8192
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Options struct {
				NumCtx int `json:"num_ctx"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path != "/api/chat" || request.Options.NumCtx != capacity {
			t.Errorf("Ollama request used capacity %d at %s", request.Options.NumCtx, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"fixture","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	}))
	defer server.Close()
	settings := config.Config{ModelProvider: "ollama", ModelName: "fixture", ModelAPIKey: "fixture-key"}
	chat, err := ai.NewChatModel(settings, server.URL, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if model.ResolveContextSize(chat, 320) != capacity {
		t.Fatal("AgentScope would use a different context capacity from the provider")
	}
	if _, err := chat.Chat(t.Context(), []*message.Msg{message.UserMsg("operator", "hello")}); err != nil {
		t.Fatal(err)
	}
	if _, err := ai.NewChatModel(settings, server.URL, 0); err == nil {
		t.Fatal("Ollama accepted an unspecified request capacity")
	}
}

func TestOpenRouterTerminalFailuresDoNotPublishTools(t *testing.T) {
	const canary = "raw-provider-error-fixture"
	for _, scenario := range []struct {
		name           string
		trailer        string
		brokenHTTPBody bool
	}{
		{name: "truncated stream"},
		{name: "EOF after finish", trailer: `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`},
		{name: "truncated HTTP body after finish", brokenHTTPBody: true, trailer: `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`},
		{name: "refusal with complete tool", trailer: `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"refusal":"refused","tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}` + "\n\ndata: [DONE]"},
		{name: "provider error", trailer: `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[],"error":{"code":502,"message":"` + canary + `"}}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if scenario.brokenHTTPBody {
					w.Header().Set("Content-Length", "4096")
				}
				_, _ = fmt.Fprint(w, `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"id":"partial","type":"function","function":{"name":"lookup","arguments":"{"}}]}}]}`+"\n\n")
				if scenario.trailer != "" {
					_, _ = fmt.Fprint(w, scenario.trailer+"\n\n")
				}
			}))
			defer provider.Close()
			chat, err := ai.NewChatModel(config.Config{ModelProvider: "openrouter", ModelName: "fixture", ModelAPIKey: "fixture-key"}, provider.URL, 64_000)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := chat.ChatStream(t.Context(), []*message.Msg{message.UserMsg("operator", "check")})
			if err != nil {
				t.Fatal(err)
			}
			var failed bool
			for chunk := range stream {
				if chunk.IsLast || len(chunk.Content) > 0 {
					t.Fatal("incomplete provider stream published a model result")
				}
				if chunk.Error != nil {
					failed = true
					if strings.Contains(chunk.Error.Error(), canary) {
						t.Fatal("stream error included the provider payload")
					}
				}
			}
			if !failed {
				t.Fatal("incomplete provider stream closed without a failure")
			}
		})
	}
}

func TestOpenRouterCancellationClosesProviderStream(t *testing.T) {
	disconnected := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"finish_reason":null,"delta":{"content":"working"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-time.After(8 * time.Second):
		}
	}))
	defer provider.Close()
	chat, err := ai.NewChatModel(config.Config{ModelProvider: "openrouter", ModelName: "fixture", ModelAPIKey: "fixture-key"}, provider.URL, 64000)
	if err != nil {
		t.Fatal(err)
	}
	input := []*message.Msg{message.UserMsg("operator", "check")}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := chat.ChatStream(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first := <-stream; first.GetTextContent() != "working" || first.IsLast {
		t.Fatal("the fixture did not open an incremental provider stream")
	}
	cancel()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close the provider request")
	}
	for chunk := range stream {
		if chunk.IsLast {
			t.Fatal("cancelled provider stream appeared to complete")
		}
	}
}

func TestProviderFailureClassification(t *testing.T) {
	const canary = "private-condition-fixture"
	for _, test := range []struct {
		provider string
		status   int
	}{
		{provider: "anthropic", status: http.StatusBadRequest},
		{provider: "openrouter", status: http.StatusBadRequest},
		{provider: "openrouter", status: http.StatusTooManyRequests},
	} {
		t.Run(fmt.Sprintf("%s/%d", test.provider, test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"maximum context length exceeded (%s)"}}`, test.status, canary)
			}))
			defer server.Close()
			chat, err := ai.NewChatModel(config.Config{ModelProvider: test.provider, ModelName: "fixture", ModelAPIKey: "fixture-key"}, server.URL, 64000)
			if err != nil {
				t.Fatal(err)
			}
			_, err = chat.Chat(t.Context(), []*message.Msg{message.UserMsg("operator", "check")})
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("provider error is absent or contains the response payload: %v", err)
			}
			switch test.status {
			case http.StatusBadRequest:
				if !errors.Is(err, aserr.ErrModelContextLimit) {
					t.Errorf("AgentScope would miss the context limit: %v", err)
				}
			case http.StatusTooManyRequests:
				if !model.IsRetryableError(err) {
					t.Errorf("AgentScope would miss provider throttling: %v", err)
				}
			}
		})
	}
}

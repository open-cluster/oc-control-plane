package openrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/openrouter"
)

func TestToolResultsContinueToAForcedConclusion(t *testing.T) {
	const continuation = "signed-reasoning-for-the-next-request"
	requests := make(chan map[string]any, 2)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		tools := request["tools"].([]any)
		name := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		toolCalls := []any{
			map[string]any{"id": "read-1", "type": "function", "function": map[string]any{"name": name, "arguments": `{"purpose":"Check workload","input":{}}`}},
			map[string]any{"id": "read-2", "type": "function", "function": map[string]any{"name": name, "arguments": `{"input":`}},
			map[string]any{"id": "premature", "type": "function", "function": map[string]any{"name": "conclude", "arguments": `{"findings":[]}`}},
		}
		if calls.Add(1) == 2 {
			toolCalls = []any{map[string]any{"id": "done", "type": "function", "function": map[string]any{"name": "conclude", "arguments": `{"findings":[]}`}}}
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{
			"id": "generation-1", "object": "chat.completion", "created": 1, "model": "z-ai/glm-4.7",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": nil, "tool_calls": toolCalls,
				"reasoning_details": []any{map[string]any{"type": "reasoning.encrypted", "data": continuation, "id": "reason-1", "format": "unknown", "index": 0}},
			}}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130,
				"prompt_tokens_details": map[string]any{"cached_tokens": 20, "cache_write_tokens": 10}, "completion_tokens_details": map[string]any{"reasoning_tokens": 10}},
		}
		if calls.Load() == 2 {
			delete(response, "usage")
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	provider, err := openrouter.New(agent.ModelConfig{
		Provider: "openrouter", Model: "z-ai/glm-4.7", Credential: agent.Secret("test-key"), BaseURL: server.URL,
	}, openrouter.Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tool := integrations.Tool{Name: "kubernetes.workload_state__0123456789abcdef0123456789abcdef", Description: "Read workload state."}
	other := tool
	other.Name = "kubernetes.workload_state__fedcba9876543210fedcba9876543210"
	prompt := agent.Prompt{
		Model: "z-ai/glm-4.7", System: []agent.Block{{Text: "Investigate safely."}}, Content: []agent.Block{{Text: "Check the workload."}},
		Tools: []integrations.ToolDefinition{tool.Definition(), other.Definition(),
			{Name: "github.read_commits"}, {Name: "github_read_commits"},
			{Name: "github." + strings.Repeat("read_", 20)}, agent.ConcludeDefinition()}, MaxOutputTokens: 32_000,
	}
	first, err := provider.Complete(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ToolCalls) != 3 || first.ToolCalls[0].Name != tool.Name || first.ToolCalls[1].ID != "read-2" || first.Stop != agent.StopToolUse {
		t.Fatalf("completion = %+v", first)
	}
	if string(first.ToolCalls[1].Arguments) != `{"input":` {
		t.Fatal("malformed Tool input was substituted instead of preserved for correlated rejection")
	}
	if first.RequestID != "generation-1" || first.Usage.Input.Tokens != 70 || first.Usage.CacheRead.Tokens != 20 || first.Usage.CacheWrite.Tokens != 10 || first.Usage.Output.Tokens != 30 || first.Usage.Reasoning.Tokens != 10 {
		t.Fatalf("usage or request identity = %+v", first)
	}
	request := <-requests
	policy := request["provider"].(map[string]any)
	if policy["allow_fallbacks"] != false || policy["require_parameters"] != true || policy["data_collection"] != "deny" || policy["zdr"] != true || policy["sort"] != "price" {
		t.Fatalf("routing policy = %v", policy)
	}
	if request["model"] != "z-ai/glm-4.7" || request["max_completion_tokens"] != float64(32_000) || request["plugins"] != nil || request["models"] != nil {
		t.Fatalf("model configuration = %v", request)
	}
	definition := request["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(definition["name"].(string)) || definition["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("function definition = %v", definition)
	}
	seenNames := make(map[string]bool)
	for _, offered := range request["tools"].([]any) {
		name := offered.(map[string]any)["function"].(map[string]any)["name"].(string)
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(name) || seenNames[name] || strings.HasPrefix(name, "oc_") {
			t.Fatalf("invalid or duplicate alias %q", name)
		}
		seenNames[name] = true
	}
	if definition["name"] != "kubernetes_workload_state__0123456789abcdef0123456789abcdef" {
		t.Fatalf("ordinary alias is unreadable: %v", definition["name"])
	}
	prompt.Turns = []agent.Turn{{Assistant: agent.AssistantTurn{Calls: first.ToolCalls, Raw: first.Raw}, ToolResults: []agent.ToolResultTurn{
		{CallID: "read-1", Content: "[run 1] unhealthy workload"}, {CallID: "read-2", Content: "[run 2] invalid Tool input", IsError: true},
		{CallID: "premature", Content: "Finalization deferred: consider the read results.", IsError: true},
	}}}
	prompt.Tools = []integrations.ToolDefinition{agent.ConcludeDefinition()}
	prompt.ForceTool = agent.ConcludeToolName
	conclusion, err := provider.Complete(context.Background(), prompt)
	if err != nil || len(conclusion.ToolCalls) != 1 || conclusion.ToolCalls[0].Name != "conclude" {
		t.Fatalf("conclusion = %+v, error = %v", conclusion, err)
	}
	if conclusion.Usage.Input.IsReported || conclusion.Usage.Output.IsReported {
		t.Fatal("absent provider usage was reported as measured zero")
	}
	followup := <-requests
	messages := followup["messages"].([]any)
	assistant := messages[2].(map[string]any)
	if assistant["reasoning_details"].([]any)[0].(map[string]any)["data"] != continuation || messages[3].(map[string]any)["tool_call_id"] != "read-1" || messages[4].(map[string]any)["tool_call_id"] != "read-2" ||
		messages[5].(map[string]any)["tool_call_id"] != "premature" || !strings.Contains(messages[5].(map[string]any)["content"].(string), "deferred") {
		t.Fatalf("continuation = %v", messages)
	}
	choice := followup["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "conclude" {
		t.Fatalf("forced choice = %v", choice)
	}
}

func TestProviderFailuresAreClassifiedWithoutSourceContent(t *testing.T) {
	const private = "test-key private-source-content"
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"credential", 401, `{"error":{"code":401,"message":"` + private + `"}}`, agent.ErrRejected},
		{"unsupported model or policy", 404, `{"error":{"code":404,"message":"` + private + `"}}`, agent.ErrRejected},
		{"rate limit", 429, `{"error":{"code":429,"message":"` + private + `"}}`, agent.ErrOutage},
		{"context", 400, `{"error":{"code":400,"message":"context length exceeded ` + private + `"}}`, agent.ErrContextWindow},
		{"invalid response", 200, `{"choices":"` + private + `"}`, agent.ErrMalformed},
		{"reported model without usage", 200, `{"id":"generation-1","object":"chat.completion","created":1,"model":"actual-model","choices":[]}`, agent.ErrMalformed},
		{"ambiguous call IDs", 200, `{"id":"generation-1","object":"chat.completion","created":1,"model":"z-ai/glm-4.7","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"same","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"same","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`, agent.ErrMalformed},
		{"unoffered Tool", 200, `{"id":"generation-1","object":"chat.completion","created":1,"model":"z-ai/glm-4.7","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"github.read_commits","arguments":"{}"}}]}}]}`, agent.ErrMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			provider, err := openrouter.New(agent.ModelConfig{
				Provider: "openrouter", Model: "z-ai/glm-4.7", Credential: agent.Secret("test-key"), BaseURL: server.URL,
			}, openrouter.Options{HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			completion, err := provider.Complete(context.Background(), agent.Prompt{Model: "z-ai/glm-4.7"})
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("provider error = %v", err)
			}
			if test.name == "credential" && completion.Model != "" {
				t.Fatal("a request failure invented a responding model")
			}
			if test.name == "reported model without usage" && (completion.Model != "actual-model" || completion.Usage.Input.IsReported) {
				t.Fatal("responding-model identity was coupled to usage availability")
			}
		})
	}
}

func TestUsageDoesNotInventMissingOrInvalidMeasurements(t *testing.T) {
	for _, test := range []struct {
		name, usage   string
		input, output agent.Count
	}{
		{"invalid breakdown", `{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":90,"cache_write_tokens":20},"completion_tokens_details":{"reasoning_tokens":31}}`, agent.Counted(100), agent.Counted(30)},
		{"missing totals", `{}`, agent.Unreported(), agent.Unreported()},
		{"reported zero", `{"prompt_tokens":0,"completion_tokens":0}`, agent.Counted(0), agent.Counted(0)},
		{"negative totals", `{"prompt_tokens":-1,"completion_tokens":-2}`, agent.Unreported(), agent.Unreported()},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"generation","model":"actual-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer"}}],"usage":`+test.usage+`}`)
			}))
			defer server.Close()
			provider, err := openrouter.New(agent.ModelConfig{
				Provider: "openrouter", Model: "custom-model", Credential: agent.Secret("test-key"), BaseURL: server.URL,
			}, openrouter.Options{HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			completion, err := provider.Complete(context.Background(), agent.Prompt{Model: "custom-model"})
			if err != nil {
				t.Fatal(err)
			}
			if completion.Model != "actual-model" || completion.Usage.Input != test.input || completion.Usage.Output != test.output ||
				completion.Usage.CacheRead.IsReported || completion.Usage.CacheWrite.IsReported || completion.Usage.Reasoning.IsReported {
				t.Fatalf("usage = %+v, model = %q", completion.Usage, completion.Model)
			}
		})
	}
}

func TestRequestDeadlineStopsTheProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	provider, err := openrouter.New(agent.ModelConfig{
		Provider: "openrouter", Model: "z-ai/glm-4.7", Credential: agent.Secret("test-key"), BaseURL: server.URL, RequestTimeout: 50 * time.Millisecond,
	}, openrouter.Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = provider.Complete(context.Background(), agent.Prompt{Model: "z-ai/glm-4.7"})
	if !errors.Is(err, agent.ErrTimeout) || time.Since(started) > time.Second {
		t.Fatalf("deadline error = %v after %v", err, time.Since(started))
	}
}

package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Native tool calling on the wire: the definitions generated from the one declarative
// contract are what this adapter sends, the tool_use blocks are what it decodes, and a
// replayed turn reaches the vendor verbatim — thinking block included, because this
// vendor requires its own thinking echoed back during a tool loop.

func toolDefinitions() []integrations.ToolDefinition {
	channelTool := integrations.Tool{
		Name: "slack.list_channels", Capability: "slack.list_channels",
		Description: "Lists the channels.", WhenToUse: "First.",
		WhenNotToUse: "Twice.", Permissions: "channels:read", Output: "Channels.",
		Arguments: []integrations.ToolArgument{{
			Name: "filter", Description: "a name filter",
			Type: integrations.FieldString, Required: true,
		}},
	}
	return []integrations.ToolDefinition{
		channelTool.Definition(),
		reasoning.ConcludeDefinition(),
	}
}

func toolPrompt() reasoning.Prompt {
	return reasoning.Prompt{
		Model:           "claude-opus-5",
		System:          []reasoning.Block{{Text: "the frozen preamble", Cache: true}},
		Content:         []reasoning.Block{{Text: "the orientation", Cache: true}},
		Tools:           toolDefinitions(),
		MaxOutputTokens: 32_000,
		Effort:          reasoning.EffortHigh,
	}
}

// streamedToolUse builds a streaming response whose content is one thinking block and
// one tool_use block.
func streamedToolUse(id, name, arguments string) *http.Response {
	events := &strings.Builder{}
	fmt.Fprintf(events, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":"+
		"{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\","+
		"\"model\":\"claude-opus-5\",\"content\":[],\"stop_reason\":null,"+
		"\"stop_sequence\":null,\"usage\":%s}}\n\n", fullUsage)
	events.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\"," +
		"\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"," +
		"\"signature\":\"\"}}\n\n")
	events.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"," +
		"\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"weighing\"}}\n\n")
	events.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"," +
		"\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-1\"}}\n\n")
	events.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"," +
		"\"index\":0}\n\n")
	fmt.Fprintf(events, "event: content_block_start\ndata: {\"type\":\"content_block_start\","+
		"\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,"+
		"\"name\":%q,\"input\":{}}}\n\n", id, name)
	fmt.Fprintf(events, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\","+
		"\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n",
		quoted(arguments))
	events.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"," +
		"\"index\":1}\n\n")
	fmt.Fprintf(events, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":"+
		"{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":%s}\n\n", fullUsage)
	events.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(events.String())),
	}
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("request-id", "req_from_provider")
	return response
}

func TestComplete_SendsTheGeneratedToolDefinitions(t *testing.T) {
	provider, round := providerUnder(t,
		streamedToolUse("toolu_1", "slack.list_channels", `{"filter":"incid"}`))

	if _, err := provider.Complete(context.Background(), toolPrompt()); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var sent struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type                 string         `json:"type"`
				Properties           map[string]any `json:"properties"`
				Required             []string       `json:"required"`
				AdditionalProperties *bool          `json:"additionalProperties"`
			} `json:"input_schema"`
		} `json:"tools"`
		ToolChoice   map[string]any   `json:"tool_choice"`
		OutputConfig map[string]any   `json:"output_config"`
		Messages     []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(round.lastBody(t)), &sent); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if len(sent.Tools) != 2 || sent.Tools[0].Name != "slack.list_channels" ||
		sent.Tools[1].Name != "conclude" {
		t.Fatalf("tools = %+v", sent.Tools)
	}
	first := sent.Tools[0]
	if !strings.Contains(first.Description, "Use when: First.") ||
		!strings.Contains(first.Description, "Returns: Channels.") {
		t.Errorf("description = %q; the composed description is what travels", first.Description)
	}
	if first.InputSchema.Type != "object" ||
		len(first.InputSchema.Required) != 1 || first.InputSchema.Required[0] != "filter" {
		t.Errorf("input schema = %+v", first.InputSchema)
	}
	if first.InputSchema.AdditionalProperties == nil || *first.InputSchema.AdditionalProperties {
		t.Error("the schema does not close over invented arguments")
	}
	if sent.ToolChoice["type"] != "auto" {
		t.Errorf("tool_choice = %v; the open turn leaves the choice to the model", sent.ToolChoice)
	}
	if _, declared := sent.OutputConfig["format"]; declared {
		t.Error("a tool-calling prompt declares no output format; the conclude tool is " +
			"the contract")
	}
}

func TestComplete_DecodesToolUseIntoCalls(t *testing.T) {
	provider, _ := providerUnder(t,
		streamedToolUse("toolu_1", "slack.list_channels", `{"filter":"incid"}`))

	completion, err := provider.Complete(context.Background(), toolPrompt())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completion.Stop != reasoning.StopToolUse {
		t.Fatalf("stop = %s", completion.Stop)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "toolu_1" ||
		completion.ToolCalls[0].Name != "slack.list_channels" {
		t.Fatalf("calls = %+v", completion.ToolCalls)
	}
	var arguments map[string]any
	if err := json.Unmarshal(completion.ToolCalls[0].Arguments, &arguments); err != nil ||
		arguments["filter"] != "incid" {
		t.Errorf("arguments = %s", completion.ToolCalls[0].Arguments)
	}
	if len(completion.Raw) == 0 {
		t.Error("the turn must be captured for verbatim replay")
	}
}

func TestComplete_ReplaysTheTurnVerbatimWithResults(t *testing.T) {
	provider, round := providerUnder(t,
		streamedToolUse("toolu_1", "slack.list_channels", `{"filter":"incid"}`),
		streamedToolUse("toolu_2", "conclude", `{"findings":[],"next_steps":[]}`))

	first, err := provider.Complete(context.Background(), toolPrompt())
	if err != nil {
		t.Fatalf("first complete: %v", err)
	}

	followUp := toolPrompt()
	followUp.Turns = []reasoning.Turn{{
		Assistant: reasoning.AssistantTurn{
			Calls: first.ToolCalls, Raw: first.Raw,
		},
		Results: []reasoning.ToolResultTurn{{
			CallID: "toolu_1", Content: "[run 1] 2 channels", IsError: false,
		}},
		Instruction: "No further reads are available. Conclude now.",
	}}
	followUp.ForceTool = reasoning.ConcludeToolName
	if _, err := provider.Complete(context.Background(), followUp); err != nil {
		t.Fatalf("second complete: %v", err)
	}

	body := round.lastBody(t)
	var sent struct {
		ToolChoice map[string]any `json:"tool_choice"`
		Messages   []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if sent.ToolChoice["type"] != "tool" || sent.ToolChoice["name"] != "conclude" {
		t.Errorf("tool_choice = %v; the forced turn names the one callable tool", sent.ToolChoice)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("messages = %d, want orientation, assistant turn, results", len(sent.Messages))
	}
	assistant := sent.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("second message role = %q", assistant.Role)
	}
	kinds := map[string]bool{}
	for _, block := range assistant.Content {
		kinds[block.Type] = true
		if block.Type == "thinking" && (block.Thinking != "weighing" ||
			block.Signature != "sig-1") {
			t.Errorf("thinking block = %+v; the vendor requires its thinking replayed "+
				"verbatim", block)
		}
		if block.Type == "tool_use" && block.ID != "toolu_1" {
			t.Errorf("tool_use id = %q", block.ID)
		}
	}
	if !kinds["thinking"] || !kinds["tool_use"] {
		t.Errorf("assistant blocks = %v; the replay lost part of the turn", kinds)
	}
	answer := sent.Messages[2]
	if answer.Role != "user" || len(answer.Content) != 2 {
		t.Fatalf("third message = %+v; tool result plus instruction", answer)
	}
	if answer.Content[0].Type != "tool_result" || answer.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("result block = %+v", answer.Content[0])
	}
	if answer.Content[1].Type != "text" ||
		!strings.Contains(answer.Content[1].Text, "Conclude now") {
		t.Errorf("instruction block = %+v", answer.Content[1])
	}
}

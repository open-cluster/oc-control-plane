package slack

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The tools against the fake vendor: bounds applied, truncation surfaced, refusals in
// plain language. The Run functions under test are the real ones the catalog serves.

func toolNamed(t *testing.T, client *Client, name string) integrations.Tool {
	t.Helper()
	for _, tool := range tools(client) {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %s", name)
	return integrations.Tool{}
}

func run(
	t *testing.T, client *Client, name string, args map[string]any,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, client, name).Run(testContext(t), integrations.ToolRequest{
		Credential: "xoxb-under-test",
		Arguments:  args,
	})
}

func TestListChannelsSelectsByNameAndTopic(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.list", `{"ok":true,"channels":[
		{"id":"C1","name":"payments-alerts","topic":{"value":""},"purpose":{"value":""}},
		{"id":"C2","name":"random","topic":{"value":"pets and lunch"},"purpose":{"value":""}},
		{"id":"C3","name":"ops","topic":{"value":"PAYMENTS incidents land here"},"purpose":{"value":""}}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.list_channels",
		map[string]any{"nameContains": "payments"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]channelContent)
	if !isTyped {
		t.Fatalf("content is %T", result.Content)
	}
	if len(selected) != 2 || selected[0].ID != "C1" || selected[1].ID != "C3" {
		t.Errorf("selection = %+v; matching reads names and topics, case-insensitively", selected)
	}
}

func TestListChannelsAppliesTheDefaultBound(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("limit"); got != "100" {
			t.Errorf("an unstated limit reached the vendor as %q, want the named default", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"channels":[]}`))
	}

	if _, err := run(t, NewClient(fake.URL), "slack.list_channels", nil); err != nil {
		t.Fatalf("listing: %v", err)
	}
}

func TestChannelHistoryRefusesAnUndeclaredArgument(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.get_channel_history",
		map[string]any{"channel": "C1", "chanel": "typo"})
	if err == nil || !strings.Contains(err.Error(), "chanel") {
		t.Fatalf("an undeclared argument must be refused by name, got %v", err)
	}
}

func TestChannelHistoryRequiresTheChannel(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.get_channel_history", nil)
	if err == nil || !strings.Contains(err.Error(), "channel is required") {
		t.Fatalf("want a refusal naming the missing channel, got %v", err)
	}
}

func TestChannelHistoryRefusesAnOversizedLimit(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.get_channel_history",
		map[string]any{"channel": "C1", "limit": float64(100000)})
	if err == nil || !strings.Contains(err.Error(), "between 1 and") {
		t.Fatalf("want a refusal naming the bound, got %v", err)
	}
}

func TestChannelHistoryRefusesAnUnreadableWindow(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.get_channel_history",
		map[string]any{"channel": "C1", "oldest": "yesterday-ish"})
	if err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("want a refusal naming the expected form, got %v", err)
	}
}

func TestChannelHistorySurfacesTruncation(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.history", `{"ok":true,"has_more":true,
		"messages":[{"ts":"1","user":"U1","text":"deploying now"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C1"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !result.Truncated {
		t.Error("the vendor said has_more and the tool did not surface it")
	}
}

func TestThreadRepliesRefuseALongThreadRatherThanShorteningIt(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.replies", `{"ok":true,"has_more":true,
		"messages":[{"ts":"1","user":"U1","text":"parent"}]}`)

	_, err := run(t, NewClient(fake.URL), "slack.get_thread_replies",
		map[string]any{"channel": "C1", "threadTs": "1"})
	if err == nil || !strings.Contains(err.Error(), "holds more than") {
		t.Fatalf("a long thread must be refused with the bound named, got %v", err)
	}
}

func TestThreadRepliesReturnAWholeThread(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.replies", `{"ok":true,"has_more":false,
		"messages":[{"ts":"1","user":"U1","text":"parent"},
		            {"ts":"2","user":"U2","text":"the fix is out"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_thread_replies",
		map[string]any{"channel": "C1", "threadTs": "1"})
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	read, isTyped := result.Content.([]messageContent)
	if !isTyped || len(read) != 2 || read[1].Text != "the fix is out" {
		t.Errorf("content = %+v", result.Content)
	}
}

func TestSearchRequiresAQuery(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.search_messages", nil)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("want a refusal naming the missing query, got %v", err)
	}
}

func TestSearchCarriesChannelNamesAndTheRemainderFlag(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("search.messages", `{"ok":true,"messages":{"total":50,"matches":[
		{"ts":"1","username":"kai","text":"payments timeout","channel":{"id":"C1","name":"incidents"}}]}}`)

	result, err := run(t, NewClient(fake.URL), "slack.search_messages",
		map[string]any{"query": "payments timeout", "limit": float64(1)})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	matches, isTyped := result.Content.([]messageContent)
	if !isTyped || len(matches) != 1 || matches[0].Channel != "incidents" {
		t.Errorf("content = %+v; a match without its channel cannot be followed", result.Content)
	}
	if !result.Truncated {
		t.Error("50 matches behind a bound of 1 must be flagged as truncated")
	}
}

func TestAVendorRefusalSurfacesFromATool(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.history", `{"ok":false,"error":"channel_not_found"}`)

	_, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C-gone"})
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("the vendor's own reason must survive to the caller, got %v", err)
	}
}

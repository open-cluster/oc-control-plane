package slack

import (
	"net/http"
	"strings"
	"testing"
	"time"

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

// resolvable adds the auth.test and users.info answers name resolution and permalinks
// read, so a message-reading test's fake speaks the whole conversation.
func resolvable(fake *fakeSlack) {
	fake.answer("auth.test",
		`{"ok":true,"team":"Acme","user":"bot","url":"https://acme.slack.com/"}`)
	fake.answer("users.info",
		`{"ok":true,"user":{"name":"kai","profile":{"display_name":"Kai","real_name":"Kai R"}}}`)
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

func TestListChannelsAsksTheVendorForFullPages(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("limit"); got != "200" {
			t.Errorf("the vendor was asked for %q, want the full page the walk reads", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"channels":[]}`))
	}

	if _, err := run(t, NewClient(fake.URL), "slack.list_channels", nil); err != nil {
		t.Fatalf("listing: %v", err)
	}
}

func TestListChannelsWalksPagesToFindAMatch(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("cursor") == "" {
			_, _ = writer.Write([]byte(`{"ok":true,"channels":[
				{"id":"C1","name":"random","topic":{"value":""},"purpose":{"value":""}}],
				"response_metadata":{"next_cursor":"page2"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"channels":[
			{"id":"C2","name":"payments-alerts","topic":{"value":""},"purpose":{"value":""}}]}`))
	}

	result, err := run(t, NewClient(fake.URL), "slack.list_channels",
		map[string]any{"nameContains": "payments"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	selected, isTyped := result.Content.([]channelContent)
	if !isTyped || len(selected) != 1 || selected[0].ID != "C2" {
		t.Fatalf("selection = %+v; a match beyond the first page must be findable", result.Content)
	}
	if result.Truncated {
		t.Error("the walk reached the workspace's end; nothing was left unread")
	}
	if fake.called("conversations.list") != 2 {
		t.Errorf("the walk made %d calls, want 2", fake.called("conversations.list"))
	}
}

func TestListChannelsFlagsAWalkStoppedAtItsPageBound(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"channels":[
			{"id":"C1","name":"random","topic":{"value":""},"purpose":{"value":""}}],
			"response_metadata":{"next_cursor":"more"}}`))
	}

	result, err := run(t, NewClient(fake.URL), "slack.list_channels",
		map[string]any{"nameContains": "payments"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if !result.Truncated {
		t.Error("a walk that stopped with pages unread must flag truncation")
	}
	if fake.called("conversations.list") != maxChannelPages {
		t.Errorf("the walk made %d calls, want its page bound %d",
			fake.called("conversations.list"), maxChannelPages)
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
	resolvable(fake)
	fake.answer("conversations.history", `{"ok":true,"has_more":true,
		"messages":[{"ts":"1","user":"U1234567","text":"deploying now"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C1"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !result.Truncated {
		t.Error("the vendor said has_more and the tool did not surface it")
	}
}

// A transcript is evidence someone can act on: authors resolve to display names with
// the raw id kept beside, and every message carries the workspace's own permalink.
func TestChannelHistoryResolvesAuthorsAndAttachesPermalinks(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	resolvable(fake)
	fake.answer("conversations.history", `{"ok":true,"has_more":false,
		"messages":[{"ts":"1767366200.000200","user":"U1234567","text":"deploying now"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C1"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	read, isTyped := result.Content.([]messageContent)
	if !isTyped || len(read) != 1 {
		t.Fatalf("content = %+v", result.Content)
	}
	if read[0].User != "Kai" || read[0].UserID != "U1234567" {
		t.Errorf("author = %q (%q); the display name resolves with the id kept",
			read[0].User, read[0].UserID)
	}
	if read[0].Permalink != "https://acme.slack.com/archives/C1/p1767366200000200" {
		t.Errorf("permalink = %q", read[0].Permalink)
	}
}

func TestChannelHistoryKeepsRawIDsWhenResolutionCannot(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":false,"error":"missing_scope"}`)
	fake.answer("users.info", `{"ok":false,"error":"missing_scope"}`)
	fake.answer("conversations.history", `{"ok":true,"has_more":false,
		"messages":[{"ts":"1","user":"U1234567","text":"deploying now"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C1"})
	if err != nil {
		t.Fatalf("a transcript with raw ids beats a failed read: %v", err)
	}
	read := result.Content.([]messageContent)
	if read[0].User != "U1234567" || read[0].Permalink != "" {
		t.Errorf("unresolvable message = %+v; the raw id stands in", read[0])
	}
}

// A refused resolution is remembered: a transcript full of one author's messages costs
// one refused users.info, not one per message.
func TestARefusedUserResolutionIsAskedOnce(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":false,"error":"missing_scope"}`)
	fake.answer("users.info", `{"ok":false,"error":"missing_scope"}`)
	fake.answer("conversations.history", `{"ok":true,"has_more":false,
		"messages":[{"ts":"1","user":"U1234567","text":"deploying"},
		            {"ts":"2","user":"U1234567","text":"deployed"}]}`)

	if _, err := run(t, NewClient(fake.URL), "slack.get_channel_history",
		map[string]any{"channel": "C1"}); err != nil {
		t.Fatalf("history: %v", err)
	}
	if calls := fake.called("users.info"); calls != 1 {
		t.Errorf("users.info was asked %d times for one author; the refusal is cacheable", calls)
	}
}

// The long thread is readable from its newest end, honestly flagged — never refused
// and never a middle presented as the end.
func TestThreadRepliesReadTheNewestTailOfALongThread(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	resolvable(fake)
	fake.answers["conversations.replies"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("cursor") == "" {
			_, _ = writer.Write([]byte(`{"ok":true,"has_more":true,
				"messages":[{"ts":"1","user":"U1234567","text":"parent"},
				            {"ts":"2","user":"U1234567","text":"early"}],
				"response_metadata":{"next_cursor":"more"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"has_more":false,
			"messages":[{"ts":"3","user":"U1234567","text":"the conclusion"}]}`))
	}

	result, err := run(t, NewClient(fake.URL), "slack.get_thread_replies",
		map[string]any{"channel": "C1", "threadTs": "1", "limit": float64(2)})
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	read := result.Content.([]messageContent)
	if len(read) != 2 || read[1].Text != "the conclusion" {
		t.Fatalf("content = %+v; the newest tail is the answer", result.Content)
	}
	if !result.Truncated {
		t.Error("a tail shorter than the thread must be flagged")
	}
	if !strings.Contains(result.Summary, "newest 2 of 3") {
		t.Errorf("summary %q does not say how much of the thread this is", result.Summary)
	}
}

func TestThreadRepliesReturnAWholeThread(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	resolvable(fake)
	fake.answer("conversations.replies", `{"ok":true,"has_more":false,
		"messages":[{"ts":"1","user":"U1234567","text":"parent"},
		            {"ts":"2","user":"U7654321","text":"the fix is out"}]}`)

	result, err := run(t, NewClient(fake.URL), "slack.get_thread_replies",
		map[string]any{"channel": "C1", "threadTs": "1"})
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	read, isTyped := result.Content.([]messageContent)
	if !isTyped || len(read) != 2 || read[1].Text != "the fix is out" {
		t.Errorf("content = %+v", result.Content)
	}
	if result.Truncated {
		t.Error("a whole thread is not truncated")
	}
}

func TestSearchRequiresAQuery(t *testing.T) {
	t.Parallel()

	_, err := run(t, NewClient("http://127.0.0.1:1"), "slack.search_messages", nil)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("want a refusal naming the missing query, got %v", err)
	}
}

func TestSearchCarriesChannelIdsAndTheRemainderFlag(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	resolvable(fake)
	fake.answer("search.messages", `{"ok":true,"messages":{"total":50,"matches":[
		{"ts":"1","username":"kai","text":"payments timeout","channel":{"id":"C1","name":"incidents"}}]}}`)

	result, err := run(t, NewClient(fake.URL), "slack.search_messages",
		map[string]any{"query": "payments timeout", "limit": float64(1)})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	matches, isTyped := result.Content.([]messageContent)
	if !isTyped || len(matches) != 1 || matches[0].Channel != "incidents" ||
		matches[0].ChannelID != "C1" {
		t.Errorf("content = %+v; a match without its channel id forces a re-listing",
			result.Content)
	}
	if result.Sources[0] != "C1" {
		t.Errorf("sources = %v; the id is what a history pivot takes", result.Sources)
	}
	if !result.Truncated {
		t.Error("50 matches behind a bound of 1 must be flagged as truncated")
	}
}

// The window travels structurally into the search query: the model asks with terms,
// and the investigation's own window becomes the date modifiers.
func TestSearchDerivesTheWindowFromTheInvestigation(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	resolvable(fake)
	fake.answers["search.messages"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if !strings.Contains(query, "after:2026-03-04") || !strings.Contains(query, "before:2026-03-07") {
			t.Errorf("query %q does not carry the window widened a day each way", query)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"messages":{"total":0,"matches":[]}}`))
	}

	tool := toolNamed(t, NewClient(fake.URL), "slack.search_messages")
	_, err := tool.Run(testContext(t), integrations.ToolRequest{
		Credential:  "xoxp-user-token",
		Arguments:   map[string]any{"query": "payments timeout"},
		WindowFrom:  time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC),
		WindowUntil: time.Date(2026, 3, 6, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
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

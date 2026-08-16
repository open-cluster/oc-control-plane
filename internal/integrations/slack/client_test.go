package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The client against a fake Slack. The fake speaks the envelope, the headers and the
// failure shapes the real API does, because the client's whole job is decoding those
// correctly — a test that mocked the client itself would prove nothing about that.

// fakeSlack is a configurable stand-in for the Slack Web API.
type fakeSlack struct {
	*httptest.Server
	// calls counts requests per method, so a test can assert a retry happened once.
	calls map[string]*atomic.Int64
	// answers maps a method ("auth.test") to what the fake returns for it.
	answers map[string]func(writer http.ResponseWriter, request *http.Request)
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()

	fake := &fakeSlack{
		calls:   map[string]*atomic.Int64{},
		answers: map[string]func(http.ResponseWriter, *http.Request){},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			method := strings.TrimPrefix(request.URL.Path, "/")
			counter, counted := fake.calls[method]
			if !counted {
				counter = &atomic.Int64{}
				fake.calls[method] = counter
			}
			counter.Add(1)

			answer, known := fake.answers[method]
			if !known {
				t.Errorf("the fake was asked for %q and has no answer for it", method)
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			answer(writer, request)
		}))
	t.Cleanup(fake.Close)
	return fake
}

// answer sets what one method returns.
func (f *fakeSlack) answer(method string, body string) {
	f.answers[method] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	}
}

func (f *fakeSlack) called(method string) int64 {
	counter, counted := f.calls[method]
	if !counted {
		return 0
	}
	return counter.Load()
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAuthTestReturnsIdentityAndScopes(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer xoxb-under-test" {
			t.Errorf("auth.test was called with authorization %q", got)
		}
		writer.Header().Set("X-OAuth-Scopes", "channels:read,channels:history")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme","user":"opencluster-bot",
			"team_id":"T123","user_id":"U456"}`))
	}

	identity, err := NewClient(fake.URL).AuthTest(testContext(t), "xoxb-under-test")
	if err != nil {
		t.Fatalf("auth.test: %v", err)
	}
	if identity.Workspace != "Acme" || identity.Bot != "opencluster-bot" {
		t.Errorf("identity = %+v", identity)
	}
	if len(identity.Scopes) != 2 || identity.Scopes[0] != "channels:read" ||
		identity.Scopes[1] != "channels:history" {
		t.Errorf("scopes = %v, want the two the header granted", identity.Scopes)
	}
}

func TestARefusedTokenIsATypedAnswerNotATransportError(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":false,"error":"invalid_auth"}`)

	_, err := NewClient(fake.URL).AuthTest(testContext(t), "xoxb-revoked")
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("a Slack refusal must surface as an APIError, got %v", err)
	}
	if refusal.Code != "invalid_auth" {
		t.Errorf("refusal code = %q", refusal.Code)
	}
}

func TestRateLimitingHonoursRetryAfterOnce(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		if fake.called("auth.test") == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"team":"Acme","user":"bot"}`))
	}

	started := time.Now()
	_, err := NewClient(fake.URL).AuthTest(testContext(t), "xoxb-under-test")
	if err != nil {
		t.Fatalf("a rate-limited read must succeed on the retry: %v", err)
	}
	if fake.called("auth.test") != 2 {
		t.Errorf("auth.test was called %d times, want exactly one retry", fake.called("auth.test"))
	}
	if time.Since(started) < time.Second {
		t.Error("the retry did not wait the Retry-After the vendor asked for")
	}
}

func TestRateLimitingRetriesOnlyOnce(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	_, err := NewClient(fake.URL).AuthTest(testContext(t), "xoxb-under-test")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("persistent rate limiting must surface as ErrRateLimited, got %v", err)
	}
	if fake.called("auth.test") != 2 {
		t.Errorf("auth.test was called %d times; one retry is the whole allowance",
			fake.called("auth.test"))
	}
}

func TestRateLimitingRefusesAWaitThatCannotFitTheBudget(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "3600")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	_, err := NewClient(fake.URL).AuthTest(ctx, "xoxb-under-test")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Error("the client slept towards a wait that could never fit the context budget")
	}
	if fake.called("auth.test") != 1 {
		t.Errorf("auth.test was called %d times; a wait past the deadline must not be attempted",
			fake.called("auth.test"))
	}
}

func TestAWaitPastTheCapIsRefusedEvenWithoutADeadline(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["auth.test"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "86400")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	started := time.Now()
	_, err := NewClient(fake.URL).AuthTest(context.Background(), "xoxb-under-test")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Error("a caller with no deadline was parked on the vendor's say-so")
	}
	if fake.called("auth.test") != 1 {
		t.Errorf("auth.test was called %d times; a day-long wait is not worth taking",
			fake.called("auth.test"))
	}
}

func TestChannelsAreListedBoundedWithTruncationFlagged(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("limit") != "2" {
			t.Errorf("conversations.list was asked for limit %q, want the caller's own bound",
				query.Get("limit"))
		}
		if query.Get("exclude_archived") != "true" {
			t.Error("archived channels were not excluded; an investigation reads live rooms")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,
			"channels":[
				{"id":"C1","name":"incidents","topic":{"value":"live incident chat"},
				 "purpose":{"value":"where alerts land"},"num_members":41},
				{"id":"C2","name":"payments-alerts","topic":{"value":""},
				 "purpose":{"value":""},"num_members":7}],
			"response_metadata":{"next_cursor":"dGVhbTpDMDM"}}`))
	}

	listed, err := NewClient(fake.URL).Channels(testContext(t), "xoxb-under-test", 2)
	if err != nil {
		t.Fatalf("listing channels: %v", err)
	}
	if len(listed.Channels) != 2 || listed.Channels[0].Name != "incidents" {
		t.Errorf("channels = %+v", listed.Channels)
	}
	if listed.Channels[0].Topic != "live incident chat" {
		t.Errorf("topic = %q; channel selection reads topics", listed.Channels[0].Topic)
	}
	if !listed.Truncated {
		t.Error("a further cursor means the listing is incomplete, and that must be flagged")
	}
}

func TestHistoryIsBoundedToTheAskedWindow(t *testing.T) {
	t.Parallel()

	oldest := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	latest := oldest.Add(30 * time.Minute)

	fake := newFakeSlack(t)
	fake.answers["conversations.history"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("channel") != "C1" {
			t.Errorf("channel = %q", query.Get("channel"))
		}
		if query.Get("oldest") != "1767366000.000000" || query.Get("latest") != "1767367800.000000" {
			t.Errorf("window = [%s, %s]; the incident's own window must bound the read",
				query.Get("oldest"), query.Get("latest"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"has_more":true,
			"messages":[
				{"ts":"1767366100.000200","user":"U1","text":"deploy finished",
				 "thread_ts":"","reply_count":0},
				{"ts":"1767366200.000200","user":"U2","text":"seeing 500s","reply_count":3,
				 "thread_ts":"1767366200.000200"}]}`))
	}

	messages, err := NewClient(fake.URL).History(testContext(t), "xoxb-under-test", HistoryQuery{
		Channel: "C1", Oldest: oldest, Latest: latest, Limit: 50,
	})
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	if len(messages.Messages) != 2 || messages.Messages[1].ReplyCount != 3 {
		t.Errorf("messages = %+v", messages.Messages)
	}
	if !messages.Truncated {
		t.Error("has_more means the window holds more than the bound; that must be flagged")
	}
}

func TestRepliesReportWhenAThreadHoldsMore(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("conversations.replies", `{"ok":true,"has_more":true,
		"messages":[{"ts":"1","user":"U1","text":"parent"}]}`)

	replies, err := NewClient(fake.URL).Replies(testContext(t), "xoxb-under-test", RepliesQuery{
		Channel: "C1", ThreadTS: "1767366200.000200", Limit: 10,
	})
	if err != nil {
		t.Fatalf("reading replies: %v", err)
	}
	if !replies.Truncated {
		t.Error("a thread longer than the bound must say so; the tool refuses on this flag")
	}
}

func TestSearchCarriesTheQueryAndFlagsTheRemainder(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["search.messages"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("query"); got != "payments timeout" {
			t.Errorf("query = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"messages":{
			"total": 231,
			"matches":[{"ts":"1767366100.000200","username":"kai","text":"payments timeout again",
			            "channel":{"id":"C1","name":"incidents"}}]}}`))
	}

	found, err := NewClient(fake.URL).Search(testContext(t), "xoxb-under-test", SearchQuery{
		Query: "payments timeout", Count: 1,
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(found.Matches) != 1 || found.Matches[0].Channel != "incidents" {
		t.Errorf("matches = %+v", found.Matches)
	}
	if !found.Truncated {
		t.Error("231 matches behind a bound of 1 is a truncated answer, and must say so")
	}
}

func TestAnOversizedAnswerIsRefusedNotSwallowed(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.history"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"messages":[{"ts":"1","text":"`))
		filler := strings.Repeat("a", 64<<10)
		for written := 0; written < maxResponseBytes; written += len(filler) {
			_, _ = writer.Write([]byte(filler))
		}
		_, _ = writer.Write([]byte(`"}]}`))
	}

	_, err := NewClient(fake.URL).History(testContext(t), "xoxb-under-test", HistoryQuery{
		Channel: "C1", Limit: 1,
	})
	if err == nil {
		t.Fatal("an answer past the response ceiling must be refused")
	}
}

// The fake above answers what it is asked; this pins that the client sends token requests
// the way Slack documents them, so the fake cannot drift into testing a dialect only this
// repository speaks.
func TestRequestsCarryTheBearerTokenAndNoQueryToken(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answers["conversations.list"] = func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("token") != "" {
			t.Error("the token travelled in the query string, where proxies and logs can read it")
		}
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
			t.Error("no bearer authorization header")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"channels":[]}`))
	}

	if _, err := NewClient(fake.URL).Channels(testContext(t), "xoxb-under-test", 10); err != nil {
		t.Fatalf("listing channels: %v", err)
	}
}

func TestTheDefaultBaseURLIsTheVendors(t *testing.T) {
	t.Parallel()

	client := NewClient("")
	if client.baseURL != "https://slack.com/api" {
		t.Errorf("default base URL = %q", client.baseURL)
	}
}

// decode is exercised through every method above; this pins the envelope rule itself: ok
// false with no error code is still a refusal, because trusting the body shape of a
// refusal is how a refusal gets read as an empty success.
func TestAnEnvelopeRefusalWithoutACodeIsStillARefusal(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":false}`)

	_, err := NewClient(fake.URL).AuthTest(testContext(t), "xoxb-under-test")
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("want an APIError, got %v", err)
	}
	if refusal.Code == "" {
		t.Error("a refusal with no code must still name one for the operator")
	}
}

// A helper the other tests lean on implicitly: the fake must be reachable and JSON-clean.
func TestFakeSpeaksJSON(t *testing.T) {
	t.Parallel()

	fake := newFakeSlack(t)
	fake.answer("auth.test", `{"ok":true,"team":"Acme","user":"bot"}`)

	response, err := http.Get(fake.URL + "/auth.test")
	if err != nil {
		t.Fatalf("reaching the fake: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("the fake's answer is not JSON: %v", err)
	}
}

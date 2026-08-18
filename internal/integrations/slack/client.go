// Package slack is the Slack provider: the Integration Type definition, the live token
// verification, and the read-only bounded tools an investigation reads channels through.
//
// The vendor's payload shapes exist inside this package and nowhere else; what leaves is
// this system's own types. Everything read here is text from a customer's workspace: it
// may be attacker-influenced and must never become an instruction, a destination, or an
// authorisation claim downstream.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is where the Web API lives. A test points the client at a fake instead,
// which is the provider transport seam: the verification and tool code under test is the
// real code, and only the far end is stood in for.
const defaultBaseURL = "https://slack.com/api"

// maxResponseBytes bounds what one answer may hold. Slack's own page limits keep real
// answers far below this; an answer that reaches it is not the API this client speaks.
const maxResponseBytes = 4 << 20

// requestTimeout is a backstop on one call. Every caller passes a bounded context; this
// exists so a caller that forgot cannot hold a connection forever.
const requestTimeout = 60 * time.Second

// maxRetryWait bounds how long a Retry-After is worth honouring. A vendor asking for
// more is answered as rate-limited now: one bounded read must not park a goroutine on
// the vendor's say-so, deadline or none.
const maxRetryWait = 30 * time.Second

// ErrRateLimited reports that Slack refused the call twice for rate, or asked for a wait
// the caller's own deadline cannot hold. The read is safe to repeat later: everything this
// client does is a read, so no retry can double an effect.
var ErrRateLimited = errors.New("slack is rate limiting this workspace's token")

// APIError is Slack's own refusal, decoded from the ok/error envelope. It is typed so
// verification can tell a revoked token from a missing scope from an unreachable vendor —
// three different answers to an operator.
type APIError struct {
	// Code is the vendor's error identifier: "invalid_auth", "missing_scope",
	// "channel_not_found".
	Code string
}

func (e *APIError) Error() string { return "slack refused the call: " + e.Code }

// Client is the one HTTP client this provider holds. One per vendor, deliberately: a
// second client is a second place a header, a bound or a retry rule could differ.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds the client. An empty base URL means the vendor's own.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Identity is who a token belongs to, from auth.test.
type Identity struct {
	// Workspace is the team name, and Bot the token's own user name — the two facts an
	// operator recognises an installation by.
	Workspace string
	Bot       string
	// Scopes is what the token was granted, read from the X-OAuth-Scopes header on the
	// answer. Verification compares it with what the tools need.
	Scopes []string
}

// Channel is one conversation as the listing reports it. Topic and purpose travel because
// channel SELECTION reads them: candidates are chosen from names and topics, never by
// scanning contents.
type Channel struct {
	ID      string
	Name    string
	Topic   string
	Purpose string
	Members int
}

// Channels is a bounded page of the workspace's channels.
type Channels struct {
	Channels []Channel
	// Truncated reports that the workspace holds more than the bound; a reader must not
	// mistake this page for the whole set.
	Truncated bool
}

// Message is one message as history, replies and search report it.
type Message struct {
	TS   string
	User string
	Text string
	// ThreadTS names the thread this message heads or belongs to; empty outside threads.
	ThreadTS   string
	ReplyCount int
	// Channel is the channel name, populated by search, whose matches span channels.
	Channel string
}

// Messages is a bounded read of a channel or thread.
type Messages struct {
	Messages  []Message
	Truncated bool
}

// HistoryQuery bounds one channel read. The window is the incident's own; a read with no
// window reads the channel's recent tail up to the limit.
type HistoryQuery struct {
	Channel string
	Oldest  time.Time
	Latest  time.Time
	Limit   int
}

// RepliesQuery bounds one thread read.
type RepliesQuery struct {
	Channel  string
	ThreadTS string
	Limit    int
}

// SearchQuery bounds one search.
type SearchQuery struct {
	Query string
	Count int
}

// SearchResults is a bounded search answer.
type SearchResults struct {
	Matches   []Message
	Truncated bool
}

// AuthTest verifies a token live and reports whose it is. This is the probe behind
// "verified means the far end answered".
func (c *Client) AuthTest(ctx context.Context, token string) (Identity, error) {
	var decoded struct {
		Team string `json:"team"`
		User string `json:"user"`
	}
	header, err := c.call(ctx, token, "auth.test", nil, &decoded)
	if err != nil {
		return Identity{}, err
	}

	identity := Identity{Workspace: decoded.Team, Bot: decoded.User}
	for scope := range strings.SplitSeq(header.Get("X-OAuth-Scopes"), ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			identity.Scopes = append(identity.Scopes, trimmed)
		}
	}
	return identity, nil
}

// Channels lists public, unarchived channels, one bounded page. Selection over the result
// reads names and topics; nothing here reads message content.
func (c *Client) Channels(ctx context.Context, token string, limit int) (Channels, error) {
	var decoded struct {
		Channels []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Topic struct {
				Value string `json:"value"`
			} `json:"topic"`
			Purpose struct {
				Value string `json:"value"`
			} `json:"purpose"`
			Members int `json:"num_members"`
		} `json:"channels"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	_, err := c.call(ctx, token, "conversations.list", url.Values{
		"limit":            {strconv.Itoa(limit)},
		"exclude_archived": {"true"},
		"types":            {"public_channel"},
	}, &decoded)
	if err != nil {
		return Channels{}, err
	}

	listed := Channels{
		Channels:  make([]Channel, 0, len(decoded.Channels)),
		Truncated: decoded.ResponseMetadata.NextCursor != "",
	}
	for _, one := range decoded.Channels {
		listed.Channels = append(listed.Channels, Channel{
			ID:      one.ID,
			Name:    one.Name,
			Topic:   one.Topic.Value,
			Purpose: one.Purpose.Value,
			Members: one.Members,
		})
	}
	return listed, nil
}

// History reads one channel inside a window, bounded, newest first as Slack returns it.
func (c *Client) History(ctx context.Context, token string, query HistoryQuery) (Messages, error) {
	parameters := url.Values{
		"channel":   {query.Channel},
		"limit":     {strconv.Itoa(query.Limit)},
		"inclusive": {"true"},
	}
	if !query.Oldest.IsZero() {
		parameters.Set("oldest", slackTimestamp(query.Oldest))
	}
	if !query.Latest.IsZero() {
		parameters.Set("latest", slackTimestamp(query.Latest))
	}
	return c.messages(ctx, token, "conversations.history", parameters)
}

// Replies reads one thread, bounded. Truncated true means the thread holds more than the
// bound — the tool above this refuses on it rather than presenting part of a thread as
// the thread.
func (c *Client) Replies(ctx context.Context, token string, query RepliesQuery) (Messages, error) {
	return c.messages(ctx, token, "conversations.replies", url.Values{
		"channel": {query.Channel},
		"ts":      {query.ThreadTS},
		"limit":   {strconv.Itoa(query.Limit)},
	})
}

// Search runs one bounded message search across the workspace.
func (c *Client) Search(ctx context.Context, token string, query SearchQuery) (SearchResults, error) {
	var decoded struct {
		Messages struct {
			Total   int `json:"total"`
			Matches []struct {
				TS       string `json:"ts"`
				Username string `json:"username"`
				Text     string `json:"text"`
				Channel  struct {
					Name string `json:"name"`
				} `json:"channel"`
			} `json:"matches"`
		} `json:"messages"`
	}
	_, err := c.call(ctx, token, "search.messages", url.Values{
		"query": {query.Query},
		"count": {strconv.Itoa(query.Count)},
	}, &decoded)
	if err != nil {
		return SearchResults{}, err
	}

	found := SearchResults{
		Matches:   make([]Message, 0, len(decoded.Messages.Matches)),
		Truncated: decoded.Messages.Total > len(decoded.Messages.Matches),
	}
	for _, one := range decoded.Messages.Matches {
		found.Matches = append(found.Matches, Message{
			TS:      one.TS,
			User:    one.Username,
			Text:    one.Text,
			Channel: one.Channel.Name,
		})
	}
	return found, nil
}

// messages is the shared shape of history and replies.
func (c *Client) messages(
	ctx context.Context, token, method string, parameters url.Values,
) (Messages, error) {
	var decoded struct {
		HasMore  bool `json:"has_more"`
		Messages []struct {
			TS         string `json:"ts"`
			User       string `json:"user"`
			Text       string `json:"text"`
			ThreadTS   string `json:"thread_ts"`
			ReplyCount int    `json:"reply_count"`
		} `json:"messages"`
	}
	if _, err := c.call(ctx, token, method, parameters, &decoded); err != nil {
		return Messages{}, err
	}

	read := Messages{
		Messages:  make([]Message, 0, len(decoded.Messages)),
		Truncated: decoded.HasMore,
	}
	for _, one := range decoded.Messages {
		read.Messages = append(read.Messages, Message{
			TS:         one.TS,
			User:       one.User,
			Text:       one.Text,
			ThreadTS:   one.ThreadTS,
			ReplyCount: one.ReplyCount,
		})
	}
	return read, nil
}

// call performs one Web API method and decodes its envelope into out, returning the
// response headers for the one caller that reads scopes from them.
//
// A 429 is retried exactly once, after the Retry-After the vendor asked for, and only when
// that wait fits the caller's own deadline. Every method this client speaks is a read, so
// the retry cannot double an effect; a second 429 is answered as ErrRateLimited rather
// than by queueing behind a vendor that has said no twice.
func (c *Client) call(
	ctx context.Context, token, method string, parameters url.Values, out any,
) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		header, wait, err := c.once(ctx, token, method, parameters, out)
		if wait == 0 || attempt == 1 {
			if wait != 0 {
				return nil, fmt.Errorf("%w: %s answered 429 twice", ErrRateLimited, method)
			}
			return header, err
		}

		if wait > maxRetryWait {
			return nil, fmt.Errorf("%w: %s asked for a %s wait, past what one read may park",
				ErrRateLimited, method, wait)
		}
		deadline, bounded := ctx.Deadline()
		if bounded && time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("%w: %s asked for a %s wait, past this call's deadline",
				ErrRateLimited, method, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// once performs one attempt. A non-zero wait reports a 429 and how long the vendor asked
// for; everything else is the final answer.
func (c *Client) once(
	ctx context.Context, token, method string, parameters url.Values, out any,
) (http.Header, time.Duration, error) {
	address := c.baseURL + "/" + method
	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building the %s request: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := c.http.Do(request)
	if err != nil {
		// The transport error is wrapped, not quoted onward to an operator: url.Error
		// carries the full URL, and the caller decides what an operator sees.
		return nil, 0, fmt.Errorf("reaching slack for %s: %w", method, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return nil, retryAfter(response.Header), nil
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading the %s answer: %w", method, err)
	}
	if len(body) > maxResponseBytes {
		return nil, 0, fmt.Errorf("the %s answer exceeds %d bytes; refusing to read further",
			method, maxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s answered %d", method, response.StatusCode)
	}

	// The envelope is read before the payload: ok false means the rest of the body is a
	// refusal's furniture, and decoding it as an answer would present a refusal as an
	// empty success.
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, 0, fmt.Errorf("the %s answer is not slack json: %w", method, err)
	}
	if !envelope.OK {
		code := envelope.Error
		if code == "" {
			code = "unnamed_error"
		}
		return nil, 0, &APIError{Code: code}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, 0, fmt.Errorf("decoding the %s answer: %w", method, err)
	}
	return response.Header, 0, nil
}

// retryAfter reads how long the vendor asked for, with a short default where the header is
// missing or unreadable.
func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds < 1 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

// slackTimestamp renders a moment the way the API takes window bounds.
func slackTimestamp(at time.Time) string {
	return fmt.Sprintf("%d.%06d", at.Unix(), at.Nanosecond()/1000)
}

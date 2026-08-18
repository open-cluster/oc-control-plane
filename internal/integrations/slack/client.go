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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// maxCachedNames bounds the user-name cache. Names are stable and small; the bound
// exists so a pathological workspace cannot grow process memory without limit. A full
// cache is cleared whole rather than evicted cleverly — resolution then costs one call
// again, which is the cheap failure.
const maxCachedNames = 4096

// Client is the one HTTP client this provider holds. One per vendor, deliberately: a
// second client is a second place a header, a bound or a retry rule could differ.
type Client struct {
	baseURL string
	http    *http.Client

	// names caches resolved user display names and urls the workspace's permalinks are
	// built from, keyed by a digest of the credential itself: a token identifies exactly
	// one workspace, so two workspaces cannot bleed into each other — and replacing an
	// integration's credential naturally stops serving the old workspace's answers,
	// which keying by integration would not. One users.info per author per process, not
	// per read.
	mu    sync.Mutex
	names map[string]string
	urls  map[string]string
}

// cacheKey derives the workspace-identity key a token's cached answers live under. A
// digest, so the plaintext credential is never held as a map key.
func cacheKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:8])
}

// NewClient builds the client. An empty base URL means the vendor's own.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
		names:   map[string]string{},
		urls:    map[string]string{},
	}
}

// Identity is who a token belongs to, from auth.test.
type Identity struct {
	// Workspace is the team name, and Bot the token's own user name — the two facts an
	// operator recognises an installation by.
	Workspace string
	Bot       string
	// URL is the workspace's own address, which is what a message permalink is built
	// from — scope-free, unlike everything else about a message.
	URL string
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

// Channels is one bounded page of the workspace's channels.
type Channels struct {
	Channels []Channel
	// NextCursor is where the next page starts; empty when this page ends the listing.
	// The caller walks by it — a single page filtered client-side is how a matching
	// channel beyond page one silently disappears.
	NextCursor string
}

// Message is one message as history, replies and search report it.
type Message struct {
	TS   string
	User string
	Text string
	// ThreadTS names the thread this message heads or belongs to; empty outside threads.
	ThreadTS   string
	ReplyCount int
	// Channel is the channel name and ChannelID its id, populated by search, whose
	// matches span channels — the id is what the pivot to history takes, unre-listed.
	Channel   string
	ChannelID string
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

// SearchQuery bounds one search. Before and After narrow it to the investigation's
// window with Slack's own date-granular modifiers, widened a day each way so the
// window's edge days are not lost to the granularity.
type SearchQuery struct {
	Query  string
	Count  int
	After  time.Time
	Before time.Time
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
		URL  string `json:"url"`
	}
	header, err := c.call(ctx, token, "auth.test", nil, &decoded)
	if err != nil {
		return Identity{}, err
	}

	identity := Identity{Workspace: decoded.Team, Bot: decoded.User, URL: decoded.URL}
	for scope := range strings.SplitSeq(header.Get("X-OAuth-Scopes"), ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			identity.Scopes = append(identity.Scopes, trimmed)
		}
	}
	return identity, nil
}

// Channels lists public, unarchived channels, one bounded page from the given cursor.
// Selection over the result reads names and topics; nothing here reads message content.
func (c *Client) Channels(
	ctx context.Context, token string, limit int, cursor string,
) (Channels, error) {
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
	parameters := url.Values{
		"limit":            {strconv.Itoa(limit)},
		"exclude_archived": {"true"},
		"types":            {"public_channel"},
	}
	if cursor != "" {
		parameters.Set("cursor", cursor)
	}
	_, err := c.call(ctx, token, "conversations.list", parameters, &decoded)
	if err != nil {
		return Channels{}, err
	}

	listed := Channels{
		Channels:   make([]Channel, 0, len(decoded.Channels)),
		NextCursor: decoded.ResponseMetadata.NextCursor,
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

// maxThreadPages bounds the tail walk: ten vendor pages of two hundred cover any
// war-room thread an investigation plausibly reads; past that the answer says the walk
// stopped rather than scanning on.
const maxThreadPages = 10

// threadPageSize is the vendor's own page ceiling for replies.
const threadPageSize = 200

// Thread is a thread's newest tail: the last messages of a walk from its start, because
// the vendor serves replies oldest-first with no way to ask for the end directly.
type Thread struct {
	// Messages is the newest tail, at most the query's limit, in order.
	Messages []Message
	// Walked is how many messages the walk saw.
	Walked int
	// WalkEnded reports the walk reached the thread's end, making Messages truly the
	// thread's newest tail. False means the page cap stopped it: Messages is the tail
	// of what was walked, and the thread continues past it.
	WalkEnded bool
}

// Replies reads one thread's newest tail, walking the vendor's oldest-first pages with
// the cursor and keeping the last messages — which is how a thread longer than any one
// page stays readable instead of refused.
func (c *Client) Replies(ctx context.Context, token string, query RepliesQuery) (Thread, error) {
	tail := Thread{}
	cursor := ""
	for page := 0; page < maxThreadPages; page++ {
		parameters := url.Values{
			"channel": {query.Channel},
			"ts":      {query.ThreadTS},
			"limit":   {strconv.Itoa(threadPageSize)},
		}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		var decoded struct {
			HasMore          bool          `json:"has_more"`
			Messages         []messageJSON `json:"messages"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if _, err := c.call(ctx, token, "conversations.replies", parameters, &decoded); err != nil {
			return Thread{}, err
		}
		tail.Walked += len(decoded.Messages)
		for _, one := range decoded.Messages {
			tail.Messages = append(tail.Messages, one.message())
		}
		if over := len(tail.Messages) - query.Limit; over > 0 {
			tail.Messages = append([]Message(nil), tail.Messages[over:]...)
		}
		cursor = decoded.ResponseMetadata.NextCursor
		if cursor == "" && !decoded.HasMore {
			tail.WalkEnded = true
			return tail, nil
		}
		if cursor == "" {
			// has_more with no cursor is not an answer this client can walk; stop and
			// report through WalkEnded rather than looping on the same page.
			return tail, nil
		}
	}
	return tail, nil
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
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"channel"`
			} `json:"matches"`
		} `json:"messages"`
	}
	// The window travels as Slack's own date-granular modifiers, widened a day each
	// way so the edge days survive the granularity.
	terms := query.Query
	if !query.After.IsZero() {
		terms += " after:" + query.After.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	}
	if !query.Before.IsZero() {
		terms += " before:" + query.Before.UTC().AddDate(0, 0, 1).Format("2006-01-02")
	}
	_, err := c.call(ctx, token, "search.messages", url.Values{
		"query": {terms},
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
			TS:        one.TS,
			User:      one.Username,
			Text:      one.Text,
			Channel:   one.Channel.Name,
			ChannelID: one.Channel.ID,
		})
	}
	return found, nil
}

// UserName resolves one user id to a display name via users.info, cached per
// credential so an investigation resolves each author once per process, not once per
// read. A user the workspace will not show answers as the empty string, never an
// error: a transcript with raw ids beats a failed read — and the vendor's own refusal
// (a user it will not show, a token without users:read) is cached too, so a transcript
// full of unresolvable authors costs one refused call, not one per message.
func (c *Client) UserName(ctx context.Context, token, user string) string {
	key := cacheKey(token) + "/" + user
	c.mu.Lock()
	name, cached := c.names[key]
	c.mu.Unlock()
	if cached {
		return name
	}

	var decoded struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if _, err := c.call(ctx, token, "users.info",
		url.Values{"user": {user}}, &decoded); err != nil {
		var refusal *APIError
		if errors.As(err, &refusal) {
			c.remember(key, "")
		}
		return ""
	}
	name = decoded.User.Profile.DisplayName
	if name == "" {
		name = decoded.User.Profile.RealName
	}
	if name == "" {
		name = decoded.User.Name
	}
	c.remember(key, name)
	return name
}

// remember stores one resolved (or refused) name inside the cache bound.
func (c *Client) remember(key, name string) {
	c.mu.Lock()
	if len(c.names) >= maxCachedNames {
		c.names = map[string]string{}
	}
	c.names[key] = name
	c.mu.Unlock()
}

// WorkspaceURL reports the workspace's own address for building permalinks, cached per
// credential. Empty when the vendor will not say; a transcript without links beats a
// failed read.
func (c *Client) WorkspaceURL(ctx context.Context, token string) string {
	key := cacheKey(token)
	c.mu.Lock()
	address, cached := c.urls[key]
	c.mu.Unlock()
	if cached {
		return address
	}
	identity, err := c.AuthTest(ctx, token)
	if err != nil {
		return ""
	}
	c.mu.Lock()
	c.urls[key] = identity.URL
	c.mu.Unlock()
	return identity.URL
}

// messageJSON is the vendor's message shape, decoded once for history and replies.
type messageJSON struct {
	TS         string `json:"ts"`
	User       string `json:"user"`
	Text       string `json:"text"`
	ThreadTS   string `json:"thread_ts"`
	ReplyCount int    `json:"reply_count"`
}

func (m messageJSON) message() Message {
	return Message{
		TS: m.TS, User: m.User, Text: m.Text, ThreadTS: m.ThreadTS,
		ReplyCount: m.ReplyCount,
	}
}

// messages is the shared shape of history reads.
func (c *Client) messages(
	ctx context.Context, token, method string, parameters url.Values,
) (Messages, error) {
	var decoded struct {
		HasMore  bool          `json:"has_more"`
		Messages []messageJSON `json:"messages"`
	}
	if _, err := c.call(ctx, token, method, parameters, &decoded); err != nil {
		return Messages{}, err
	}

	read := Messages{
		Messages:  make([]Message, 0, len(decoded.Messages)),
		Truncated: decoded.HasMore,
	}
	for _, one := range decoded.Messages {
		read.Messages = append(read.Messages, one.message())
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

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

const defaultBaseURL = "https://slack.com/api"

const maxResponseBytes = 4 << 20

const requestTimeout = 60 * time.Second

const maxRetryWait = 30 * time.Second

var ErrRateLimited = errors.New("slack is rate limiting this workspace's token")

type APIError struct {
	Code string
}

func (e *APIError) Error() string { return "slack refused the call: " + e.Code }

const maxCachedNames = 4096

type Client struct {
	baseURL string
	http    *http.Client

	mu    sync.Mutex
	names map[string]string
	urls  map[string]string
}

func cacheKey(token string) string {
	// Cache by credential identity without retaining the plaintext token as a map key.
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:8])
}

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

type Identity struct {
	Workspace   string
	Bot         string
	WorkspaceID string
	BotUserID   string
	URL         string
	Scopes      []string
}

type Channel struct {
	ID      string
	Name    string
	Topic   string
	Purpose string
	Members int
}

type Channels struct {
	Channels   []Channel
	NextCursor string
}

type Message struct {
	TS         string
	User       string
	Text       string
	ThreadTS   string
	ReplyCount int
	Channel    string
	ChannelID  string
}

type Messages struct {
	Messages  []Message
	Truncated bool
}

type HistoryQuery struct {
	Channel string
	Oldest  time.Time
	Latest  time.Time
	Limit   int
}

type RepliesQuery struct {
	Channel  string
	ThreadTS string
	Limit    int
}

type SearchQuery struct {
	Query  string
	Count  int
	After  time.Time
	Before time.Time
}

type SearchResults struct {
	Matches   []Message
	Truncated bool
}

func (c *Client) AuthTest(ctx context.Context, token string) (Identity, error) {
	var decoded struct {
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
		URL    string `json:"url"`
	}
	header, err := c.call(ctx, token, "auth.test", nil, &decoded)
	if err != nil {
		return Identity{}, err
	}

	identity := Identity{
		Workspace:   decoded.Team,
		Bot:         decoded.User,
		WorkspaceID: decoded.TeamID,
		BotUserID:   decoded.UserID,
		URL:         decoded.URL,
	}
	for scope := range strings.SplitSeq(header.Get("X-OAuth-Scopes"), ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			identity.Scopes = append(identity.Scopes, trimmed)
		}
	}
	return identity, nil
}

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

func (c *Client) History(ctx context.Context, token string, query HistoryQuery) (Messages, error) {
	parameters := url.Values{
		"channel":   {query.Channel},
		"limit":     {strconv.Itoa(query.Limit)},
		"inclusive": {"true"},
	}
	if !query.Oldest.IsZero() {
		parameters.Set("oldest", slackTimestamp(query.Oldest.Add(time.Microsecond-time.Nanosecond)))
	}
	if !query.Latest.IsZero() {
		// Slack includes both microsecond-granular bounds; our window end is exclusive.
		parameters.Set("latest", slackTimestamp(query.Latest.Add(-time.Nanosecond)))
	}
	return c.messages(ctx, token, "conversations.history", parameters)
}

const maxThreadPages = 10

const threadPageSize = 200

type Thread struct {
	Messages  []Message
	Walked    int
	WalkEnded bool
}

func (c *Client) Replies(ctx context.Context, token string, query RepliesQuery) (Thread, error) {
	tail := Thread{}
	cursor := ""
	for range maxThreadPages {
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
			return tail, nil
		}
	}
	return tail, nil
}

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

func (c *Client) remember(key, name string) {
	c.mu.Lock()
	if len(c.names) >= maxCachedNames {
		c.names = map[string]string{}
	}
	c.names[key] = name
	c.mu.Unlock()
}

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

type messageJSON struct {
	TS         string `json:"ts"`
	User       string `json:"user"`
	Text       string `json:"text"`
	ThreadTS   string `json:"thread_ts"`
	ReplyCount int    `json:"reply_count"`
}

func (m messageJSON) message() Message {
	return Message{
		TS:         m.TS,
		User:       m.User,
		Text:       m.Text,
		ThreadTS:   m.ThreadTS,
		ReplyCount: m.ReplyCount,
	}
}

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

func (c *Client) call(
	ctx context.Context, token, method string, parameters url.Values, out any,
) (http.Header, error) {
	return c.exchange(ctx, token, method, parameters, nil, out)
}

func (c *Client) exchange(
	ctx context.Context, token, method string, parameters, form url.Values, out any,
) (http.Header, error) {
	// Retry one vendor-directed wait only; all writes identify an existing Slack message, so
	// repeating them cannot create a second message.
	for attempt := 0; ; attempt++ {
		header, wait, err := c.once(ctx, token, method, parameters, form, out)
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

func (c *Client) request(
	ctx context.Context, token, method string, parameters, form url.Values,
) (*http.Request, error) {
	address := c.baseURL + "/" + method
	var (
		request *http.Request
		err     error
	)
	if form == nil {
		if len(parameters) > 0 {
			address += "?" + parameters.Encode()
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	} else {
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, address,
			strings.NewReader(form.Encode()))
	}
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	}
	return request, nil
}

func (c *Client) once(
	ctx context.Context, token, method string, parameters, form url.Values, out any,
) (http.Header, time.Duration, error) {
	request, err := c.request(ctx, token, method, parameters, form)
	if err != nil {
		return nil, 0, err
	}

	response, err := c.http.Do(request)
	if err != nil {
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

func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds < 1 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

func slackTimestamp(at time.Time) string {
	return fmt.Sprintf("%d.%06d", at.Unix(), at.Nanosecond()/1000)
}

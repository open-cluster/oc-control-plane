package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// THE ONLY WRITES THIS PRODUCT PERFORMS IN A CUSTOMER'S WORKSPACE.
//
// The agent's own replies, in the thread it was addressed in, and nothing else. No reactions,
// no channel joins, no posting into channels OpenCluster was not spoken to in, no editing
// anybody's message but its own stream. Those are out of scope by decision, and the decision is
// enforced by this file being the whole write surface — a reviewer reads one file to know what
// OpenCluster can say in somebody's Slack.
//
// They are COLLABORATION writes, and the vocabulary matters: distinct from external reads, and
// firmly distinct from production or remediation writes, which remain unsupported.
//
// STREAMING, AND WHY THERE IS A FALLBACK. Slack's agent platform offers native streaming so a
// reply appears as one message that fills in rather than twenty separate posts. Its method
// names are moving quickly, and the fallback is not a nicety: where those methods are not
// available to an installation, delivery uses one placeholder message updated in place — never
// a series of posts, which is what makes a channel unreadable and is the failure the whole
// stream design exists to avoid.
//
// VERIFY THE STREAMING METHOD NAMES BELOW against current Slack documentation before a
// release. The fallback path uses chat.postMessage and chat.update, which are stable.

// The streaming methods, named once. A workspace that does not offer them answers with a
// refusal this file recognises, and delivery drops to the fallback for the rest of the turn.
const (
	methodStartStream  = "chat.startStream"
	methodAppendStream = "chat.appendStream"
	methodStopStream   = "chat.stopStream"
	methodPostMessage  = "chat.postMessage"
	methodUpdate       = "chat.update"
)

// ErrStreamingUnavailable reports an installation that cannot stream: the method is unknown to
// this workspace's Slack, or the token was not granted what it needs. It is not a failure —
// delivery continues in place — so it is its own value rather than an APIError the caller would
// have to pattern-match a code out of.
var ErrStreamingUnavailable = errors.New("slack: this installation cannot stream a reply")

// unavailableCodes are the refusals that mean "not this installation" rather than "not this
// request". A code outside the set is a real failure and is returned as one.
var unavailableCodes = map[string]bool{
	"unknown_method":         true,
	"method_not_supported":   true,
	"method_deprecated":      true,
	"missing_scope":          true,
	"not_allowed_token_type": true,
}

// Reply is one visible message in a thread: the stream, or the placeholder standing in for it.
type Reply struct {
	// Channel and Thread are where it lives. Thread is the thread the question was asked
	// in, so an answer never leaves the conversation it belongs to.
	Channel string
	Thread  string
	// TS identifies the visible message. Every append and every edit names it, which is
	// what makes a retry append rather than repost.
	TS string
	// Streaming reports that this reply is a native stream rather than a placeholder being
	// edited in place.
	Streaming bool
}

// StartReply opens the turn's one visible message.
//
// It tries the native stream first and falls back to a placeholder message posted once and
// then edited. Either way the result is ONE message: a reader watching a thread sees an answer
// arriving, not a transcript of our internal progress.
func (c *Client) StartReply(
	ctx context.Context, token, channel, thread, opening string,
) (Reply, error) {
	streamed, err := c.post(ctx, token, methodStartStream, url.Values{
		"channel":   {channel},
		"thread_ts": {thread},
	})
	switch {
	case err == nil && streamed.TS != "":
		reply := Reply{Channel: channel, Thread: thread, TS: streamed.TS, Streaming: true}
		if opening != "" {
			if err := c.AppendReply(ctx, token, reply, opening); err != nil {
				return reply, err
			}
		}
		return reply, nil
	case err != nil && !isUnavailable(err):
		return Reply{}, err
	}

	// The fallback. One placeholder, posted once, edited in place from here on.
	posted, err := c.post(ctx, token, methodPostMessage, url.Values{
		"channel":   {channel},
		"thread_ts": {thread},
		"text":      {placeholder(opening)},
	})
	if err != nil {
		return Reply{}, err
	}
	return Reply{Channel: channel, Thread: thread, TS: posted.TS}, nil
}

// AppendReply adds text to a stream. Only valid on a streaming reply; the placeholder path
// uses ReplaceReply, because editing in place means sending the whole text each time.
func (c *Client) AppendReply(ctx context.Context, token string, reply Reply, text string) error {
	if text == "" {
		return nil
	}
	_, err := c.post(ctx, token, methodAppendStream, url.Values{
		"channel":       {reply.Channel},
		"ts":            {reply.TS},
		"markdown_text": {text},
	})
	return err
}

// ReplaceReply rewrites the placeholder's whole visible text.
//
// The whole text, every time, because that is what editing in place means. It is why the
// caller keeps what it has already rendered: an edit that sent only the new part would erase
// everything before it.
func (c *Client) ReplaceReply(ctx context.Context, token string, reply Reply, text string) error {
	_, err := c.post(ctx, token, methodUpdate, url.Values{
		"channel": {reply.Channel},
		"ts":      {reply.TS},
		"text":    {placeholder(text)},
	})
	return err
}

// StopReply closes the stream. It is idempotent at the vendor for a stream already stopped,
// and a no-op for a placeholder, which has nothing to close.
func (c *Client) StopReply(ctx context.Context, token string, reply Reply) error {
	if !reply.Streaming {
		return nil
	}
	_, err := c.post(ctx, token, methodStopStream, url.Values{
		"channel": {reply.Channel},
		"ts":      {reply.TS},
	})
	if err != nil && isUnavailable(err) {
		return nil
	}
	return err
}

// placeholder is what an empty reply says, because Slack will not post an empty message and a
// turn that has produced no text yet still needs its one visible message to exist.
func placeholder(text string) string {
	if text == "" {
		return "_Working on it…_"
	}
	return text
}

// isUnavailable reports a refusal meaning "not this installation".
func isUnavailable(err error) bool {
	var refusal *APIError
	return errors.As(err, &refusal) && unavailableCodes[refusal.Code]
}

// written is the part of a write's answer this package reads.
type written struct {
	TS string `json:"ts"`
}

// post performs one write. It is separate from the read path because a write is a POST with a
// form body — a Slack write sent as a query string is a message body in an access log — and
// because it must never be retried blindly: this client's reads are safe to repeat and a post
// is not, so a rate limit here is honoured ONCE and then reported.
func (c *Client) post(
	ctx context.Context, token, method string, parameters url.Values,
) (written, error) {
	for attempt := 0; ; attempt++ {
		answer, wait, err := c.postOnce(ctx, token, method, parameters)
		if wait == 0 {
			return answer, err
		}
		if attempt == 1 {
			return written{}, fmt.Errorf("%w: %s answered 429 twice", ErrRateLimited, method)
		}
		// The interval Slack asked for, honoured as asked. Waiting less is how a client
		// gets throttled into failure.
		if wait > maxRetryWait {
			return written{}, fmt.Errorf("%w: %s asked for a %s wait",
				ErrRateLimited, method, wait)
		}
		if deadline, bounded := ctx.Deadline(); bounded &&
			time.Now().Add(wait).After(deadline) {
			return written{}, fmt.Errorf("%w: %s asked for a %s wait, past this call's deadline",
				ErrRateLimited, method, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return written{}, ctx.Err()
		}
	}
}

func (c *Client) postOnce(
	ctx context.Context, token, method string, parameters url.Values,
) (written, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/"+method, bytes.NewBufferString(parameters.Encode()))
	if err != nil {
		return written{}, 0, fmt.Errorf("building the %s request: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	response, err := c.http.Do(request)
	if err != nil {
		// Wrapped rather than quoted onward: url.Error carries the full URL, and what an
		// operator sees is the caller's decision.
		return written{}, 0, fmt.Errorf("reaching slack for %s: %w", method, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return written{}, retryAfter(response.Header), nil
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return written{}, 0, fmt.Errorf("reading the %s answer: %w", method, err)
	}
	if len(body) > maxResponseBytes {
		return written{}, 0, fmt.Errorf("the %s answer exceeds %d bytes", method, maxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return written{}, 0, fmt.Errorf("%s answered %d", method, response.StatusCode)
	}

	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
		// Some writes name the message under its own key rather than at the top level.
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return written{}, 0, fmt.Errorf("the %s answer is not slack json: %w", method, err)
	}
	if !envelope.OK {
		code := envelope.Error
		if code == "" {
			code = "unnamed_error"
		}
		return written{}, 0, &APIError{Code: code}
	}
	stamp := envelope.TS
	if stamp == "" {
		stamp = envelope.Message.TS
	}
	return written{TS: stamp}, 0, nil
}

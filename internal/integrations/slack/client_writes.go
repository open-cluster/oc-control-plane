package slack

import (
	"context"
	"errors"
	"net/url"
)

// THE ONLY WRITES THIS PRODUCT PERFORMS IN A CUSTOMER'S WORKSPACE.
//
// The agent's own replies, in the thread it was addressed in, and nothing else. No reactions,
// no channel joins, no posting into channels OpenCluster was not spoken to in, no editing
// anybody's message but its own. Those are out of scope by decision, and the decision is
// enforced by this file being the whole write surface — a reviewer reads one file to know what
// OpenCluster can say in somebody's Slack.
//
// They are COLLABORATION writes, and the vocabulary matters: distinct from external reads, and
// firmly distinct from production or remediation writes, which remain unsupported. Every one of
// them is recorded as such in the audit trail by the worker that makes it.
//
// STREAMING, AND WHY THERE IS A FALLBACK. Slack's agent platform offers native streaming so a
// reply appears as one message that fills in rather than twenty separate posts. Its method
// names are moving quickly, and the fallback is not a nicety: where those methods are not
// available to an installation, the reply is one placeholder message updated in place — never a
// series of posts, which is what makes a channel unreadable and is the failure the whole design
// exists to avoid.
//
// VERIFY THE STREAMING METHOD NAMES BELOW against current Slack documentation before a release.
// The fallback path uses chat.postMessage and chat.update, which are stable.

// The methods, named once. A workspace that does not offer the streaming ones answers with a
// refusal this file recognises, and the reply falls back for the rest of the turn.
const (
	methodStartStream  = "chat.startStream"
	methodAppendStream = "chat.appendStream"
	methodStopStream   = "chat.stopStream"
	methodPostMessage  = "chat.postMessage"
	methodUpdate       = "chat.update"
)

// unavailableCodes are the refusals that mean "not this installation" rather than "not this
// request". A code outside the set is a real failure and is returned as one.
var unavailableCodes = map[string]bool{
	"unknown_method":         true,
	"method_not_supported":   true,
	"method_deprecated":      true,
	"missing_scope":          true,
	"not_allowed_token_type": true,
}

// Stream is the one visible message a turn is written into: Slack's native stream, or the
// placeholder standing in for it.
type Stream struct {
	// Channel and Thread are where it lives. Thread is the thread the question was asked
	// in, so an answer never leaves the conversation it belongs to.
	Channel string
	Thread  string
	// TS identifies the visible message. Every append and every edit names it, which is
	// what makes a retry append rather than repost.
	TS string
	// Native reports Slack's own streaming rather than a placeholder edited in place. The
	// two are different writes and must not be confused: appending to a placeholder would
	// erase the answer, and replacing a stream would repeat it.
	Native bool
}

// Held reports whether this value names a visible message at all.
func (s Stream) Held() bool { return s.TS != "" }

// StartStream opens the turn's one visible message, EMPTY.
//
// Empty on purpose. The caller records the message's identity before any content is sent, so a
// process that dies immediately afterwards resumes into the message it already opened rather
// than opening a second one — and an opening that carried content would put that content
// outside the identity's protection.
func (c *Client) StartStream(ctx context.Context, token, channel, thread string) (Stream, error) {
	streamed, err := c.write(ctx, token, methodStartStream, url.Values{
		"channel":   {channel},
		"thread_ts": {thread},
	})
	switch {
	case err == nil && streamed.TS != "":
		return Stream{Channel: channel, Thread: thread, TS: streamed.TS, Native: true}, nil
	case err != nil && !isUnavailable(err):
		return Stream{}, err
	}

	// The fallback. One placeholder, posted once, edited in place from here on.
	posted, err := c.write(ctx, token, methodPostMessage, url.Values{
		"channel":   {channel},
		"thread_ts": {thread},
		"text":      {placeholder("")},
	})
	if err != nil {
		return Stream{}, err
	}
	return Stream{Channel: channel, Thread: thread, TS: posted.TS}, nil
}

// AppendStream adds text to a native stream. The placeholder path uses ReplaceStream instead,
// because editing in place means sending the whole text each time.
func (c *Client) AppendStream(
	ctx context.Context, token string, stream Stream, text string,
) error {
	if text == "" {
		return nil
	}
	_, err := c.write(ctx, token, methodAppendStream, url.Values{
		"channel":       {stream.Channel},
		"ts":            {stream.TS},
		"markdown_text": {text},
	})
	return err
}

// ReplaceStream rewrites the placeholder's whole visible text.
//
// The whole text, every time, because that is what editing in place means. It is why the caller
// re-renders everything the turn has said: an edit carrying only the new part would erase
// everything before it.
func (c *Client) ReplaceStream(
	ctx context.Context, token string, stream Stream, text string,
) error {
	_, err := c.write(ctx, token, methodUpdate, url.Values{
		"channel": {stream.Channel},
		"ts":      {stream.TS},
		"text":    {placeholder(text)},
	})
	return err
}

// StopStream closes a native stream, and is a no-op for a placeholder, which has nothing to
// close. A stream the vendor has already stopped is not an error worth reporting.
func (c *Client) StopStream(ctx context.Context, token string, stream Stream) error {
	if !stream.Native {
		return nil
	}
	_, err := c.write(ctx, token, methodStopStream, url.Values{
		"channel": {stream.Channel},
		"ts":      {stream.TS},
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

// written is the part of a write's answer this package reads: which message it wrote.
type written struct {
	TS string `json:"ts"`
	// Some writes name the message under its own key rather than at the top level.
	Message struct {
		TS string `json:"ts"`
	} `json:"message"`
}

// write performs one write through the same transport, retry policy and envelope handling
// every read goes through. The form body is what makes it a POST; see exchange.
func (c *Client) write(
	ctx context.Context, token, method string, parameters url.Values,
) (written, error) {
	var answer written
	if _, err := c.exchange(ctx, token, method, nil, parameters, &answer); err != nil {
		return written{}, err
	}
	if answer.TS == "" {
		answer.TS = answer.Message.TS
	}
	return answer, nil
}

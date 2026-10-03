package slack

import (
	"context"
	"errors"
	"net/url"
)

const (
	methodStartStream  = "chat.startStream"
	methodAppendStream = "chat.appendStream"
	methodStopStream   = "chat.stopStream"
	methodPostMessage  = "chat.postMessage"
	methodUpdate       = "chat.update"
)

var unavailableCodes = map[string]bool{
	"unknown_method":         true,
	"method_not_supported":   true,
	"method_deprecated":      true,
	"missing_scope":          true,
	"not_allowed_token_type": true,
}

type Stream struct {
	Channel string
	Thread  string
	TS      string
	Native  bool
}

func (s Stream) Held() bool { return s.TS != "" }

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

func (c *Client) StopStream(ctx context.Context, token string, stream Stream) error {
	if !stream.Native {
		return nil
	}
	_, err := c.write(ctx, token, methodStopStream, url.Values{
		"channel": {stream.Channel},
		"ts":      {stream.TS},
	})
	var refusal *APIError
	if isUnavailable(err) || (errors.As(err, &refusal) && refusal.Code == "message_not_in_streaming_state") {
		return nil
	}
	return err
}

func placeholder(text string) string {
	if text == "" {
		return "_Working on it…_"
	}
	return text
}

func isUnavailable(err error) bool {
	var refusal *APIError
	return errors.As(err, &refusal) && unavailableCodes[refusal.Code]
}

type written struct {
	TS      string `json:"ts"`
	Message struct {
		TS string `json:"ts"`
	} `json:"message"`
}

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

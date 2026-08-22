package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// WHAT ARRIVES FROM A WORKSPACE, AND WHAT MAY BE BELIEVED ABOUT IT.
//
// This file is parsing and authenticity, and deliberately nothing else. It performs no I/O,
// resolves no tenant and decides nothing about what to do with a message — those belong to
// the surface that receives it, which already bounds bodies, already rate-limits per source
// and already owns the delivery record deduplication reuses.
//
// EVERY STEP IS A REFUSAL RATHER THAN A WARNING. A request that fails any of them is not
// processed in a degraded way; it is turned away, and the answer says as little as possible
// about which step it failed.
//
// All Slack text — message bodies, channel topics, display names — is UNTRUSTED for its whole
// life. It reaches a model as evidence about what somebody typed, never as an instruction.
// Nothing in this file makes a decision from message text.

// SignatureHeader and TimestampHeader are what Slack signs a request with.
const (
	SignatureHeader = "X-Slack-Signature"
	TimestampHeader = "X-Slack-Request-Timestamp"
)

// signatureVersion is the version prefix of Slack's signing base string. It is part of the
// signed material, so a future version cannot be silently accepted under this one's rules.
const signatureVersion = "v0"

// ReplayWindow is how far a request's timestamp may be from now, in either direction.
//
// Five minutes is Slack's own published guidance. In EITHER direction on purpose: a request
// stamped in the future is not a delayed delivery, it is a clock this deployment cannot
// reconcile or a value somebody chose, and accepting it would widen the replay window by
// however far ahead they chose to stamp it.
const ReplayWindow = 5 * time.Minute

// The refusals this file produces. They are separate values because the SURFACE logs them
// apart — an operator debugging a broken app registration needs to know a signature failed
// rather than a timestamp — while the answer sent back is one status either way.
var (
	// ErrNotSigned reports a request carrying no signature or no timestamp at all.
	ErrNotSigned = errors.New("slack: the request carries no signature")
	// ErrBadSignature reports a signature that does not match the body under this
	// deployment's signing secret. A body mutated after signing, a wrong secret and a
	// forged request all arrive as this.
	ErrBadSignature = errors.New("slack: the signature does not match the body")
	// ErrStale reports a timestamp outside the replay window.
	ErrStale = errors.New("slack: the request timestamp is outside the replay window")
	// ErrNotUnderstood reports a body that is not an events payload.
	ErrNotUnderstood = errors.New("slack: the body is not an events payload")
)

// Verify checks Slack's signature over the UNMODIFIED raw body.
//
// The body must be the bytes as received. A body re-encoded, re-indented or round-tripped
// through a decoder is a different body, and verifying that one would be verifying something
// the far end never signed — which is the whole failure this check exists to prevent.
//
// The comparison is constant-time. The timestamp is checked as part of the same call rather
// than left to a caller, because the two together are the anti-replay property and a caller
// who did one is exactly as unprotected as one who did neither.
func Verify(signingSecret string, header, timestamp string, body []byte, now time.Time) error {
	if signingSecret == "" {
		// A deployment that serves this endpoint without a signing secret would accept
		// anything. The endpoint is not served in that case; this is the second lock.
		return ErrNotSigned
	}
	if header == "" || timestamp == "" {
		return ErrNotSigned
	}

	seconds, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return ErrNotSigned
	}
	if delta := now.Sub(time.Unix(seconds, 0)); delta > ReplayWindow || delta < -ReplayWindow {
		return ErrStale
	}

	mac := hmac.New(sha256.New, []byte(signingSecret))
	// Written in three parts rather than concatenated, so the raw body is never copied into
	// a second buffer for the sake of the check.
	mac.Write([]byte(signatureVersion + ":" + timestamp + ":"))
	mac.Write(body)
	expected := signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(header)) {
		return ErrBadSignature
	}
	return nil
}

// Envelope is one events request, already parsed and not yet judged.
type Envelope struct {
	// Challenge is Slack's URL verification. Present only on that request, which is
	// answered and creates nothing.
	Challenge string
	// Application, Enterprise and Workspace are the installation this event resolves
	// through. They are read from the ENVELOPE rather than from the event, because the
	// envelope is what Slack signs the request as being about.
	Application string
	Enterprise  string
	Workspace   string
	// Event is the thing that happened, empty for a request that carries none.
	Event Event
}

// Event is one workspace happening, in the fields this product reads.
type Event struct {
	Kind string
	// Channel is where it was said, and TS its own timestamp. ThreadTS names the thread it
	// belongs to, and is empty for a message that started none.
	Channel     string
	TS          string
	ThreadTS    string
	User        string
	Text        string
	ChannelKind string
	// BotID and Subtype are what distinguishes a person speaking from an app posting and
	// from an edit, a join or a file comment. Both are read before anything else.
	BotID   string
	Subtype string
	// BotUserID is the app user an event was delivered on behalf of, when Slack said so.
	BotUserID string
}

// Parse reads an events request. It is called only after Verify has accepted the body.
func Parse(body []byte) (Envelope, error) {
	var decoded struct {
		Type           string `json:"type"`
		Challenge      string `json:"challenge"`
		APIAppID       string `json:"api_app_id"`
		TeamID         string `json:"team_id"`
		EnterpriseID   any    `json:"enterprise_id"`
		Authorizations []struct {
			EnterpriseID string `json:"enterprise_id"`
			TeamID       string `json:"team_id"`
			UserID       string `json:"user_id"`
			IsBot        bool   `json:"is_bot"`
		} `json:"authorizations"`
		Event struct {
			Type        string `json:"type"`
			Channel     string `json:"channel"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			User        string `json:"user"`
			Text        string `json:"text"`
			ChannelType string `json:"channel_type"`
			BotID       string `json:"bot_id"`
			Subtype     string `json:"subtype"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Envelope{}, ErrNotUnderstood
	}

	envelope := Envelope{
		Challenge:   decoded.Challenge,
		Application: decoded.APIAppID,
		Workspace:   decoded.TeamID,
	}
	// enterprise_id arrives as a string on a grid install and as an object on some
	// payloads. Read defensively rather than strictly: a shape this build does not expect
	// must not turn a real event into a refusal, and the value is only ever used as half a
	// lookup key.
	switch typed := decoded.EnterpriseID.(type) {
	case string:
		envelope.Enterprise = typed
	case map[string]any:
		if id, ok := typed["id"].(string); ok {
			envelope.Enterprise = id
		}
	}

	// The authorizations block is what Slack says this event was delivered to. It is the
	// more reliable source for a grid install, where the top-level team can be the
	// enterprise rather than the workspace.
	for _, authorized := range decoded.Authorizations {
		if authorized.TeamID != "" {
			envelope.Workspace = authorized.TeamID
		}
		if authorized.EnterpriseID != "" {
			envelope.Enterprise = authorized.EnterpriseID
		}
		if authorized.IsBot && authorized.UserID != "" {
			envelope.Event.BotUserID = authorized.UserID
		}
		break
	}

	if decoded.Type == "url_verification" {
		if envelope.Challenge == "" {
			return Envelope{}, ErrNotUnderstood
		}
		return envelope, nil
	}

	envelope.Event.Kind = decoded.Event.Type
	envelope.Event.Channel = decoded.Event.Channel
	envelope.Event.TS = decoded.Event.TS
	envelope.Event.ThreadTS = decoded.Event.ThreadTS
	envelope.Event.User = decoded.Event.User
	envelope.Event.Text = decoded.Event.Text
	envelope.Event.ChannelKind = decoded.Event.ChannelType
	envelope.Event.BotID = decoded.Event.BotID
	envelope.Event.Subtype = decoded.Event.Subtype
	return envelope, nil
}

// Key is the installation this envelope resolves through.
func (e Envelope) Key() InstallationKey {
	return InstallationKey{
		Application: e.Application, Enterprise: e.Enterprise, Workspace: e.Workspace,
	}
}

// InstallationKey mirrors the neutral key the persistence layer resolves by, so this package
// can compose one without importing the shape of a lookup it does not perform.
type InstallationKey struct {
	Application string
	Enterprise  string
	Workspace   string
}

// AddressedToUs reports whether this event is a person speaking TO OpenCluster.
//
// Everything else is acknowledged and discarded: channel joins, edits, reactions, file
// comments, messages from other apps, and — before anything else — OpenCluster's own
// messages. That last one is not a nicety. An agent that answers its own message in a thread
// answers its answer, and keeps going until a rate limit stops it.
//
// agent is the identity this installation answers as, from the routing record.
func (e Envelope) AddressedToUs(agent string) bool {
	event := e.Event
	switch {
	case event.BotID != "":
		// Any app, including this one. An app posting is not a person asking.
		return false
	case agent != "" && event.User == agent:
		return false
	case event.User == "":
		// Nobody said it. A message with no author is a system notice.
		return false
	case event.Subtype != "":
		// An edit, a join, a channel purpose change, a file share. Subtyped messages are
		// events ABOUT a channel rather than something said to us.
		return false
	case strings.TrimSpace(event.Text) == "":
		return false
	}

	switch event.Kind {
	case "app_mention":
		// Somebody named OpenCluster in a channel it is in.
		return true
	case "message":
		// In a direct message with the agent, every message is addressed to it; in a
		// channel, only a mention is, and a mention arrives as app_mention instead.
		return event.ChannelKind == "im"
	default:
		return false
	}
}

// Thread is where a reply belongs: the thread this message is in, or the message itself when
// it started none. Keying a new conversation on the mention's own timestamp is what makes
// "start a thread" and "continue a thread" one path with one key.
func (e Envelope) Thread() string {
	if e.Event.ThreadTS != "" {
		return e.Event.ThreadTS
	}
	return e.Event.TS
}

// Subject names the conversation this message opens, because nobody types a subject into a
// chat box. It is the message's own first line, bounded — untrusted text, used only as a
// label a person reads back.
func Subject(text string) string {
	trimmed := strings.TrimSpace(text)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
	// Mentions render as <@U123> and are noise at the front of a subject.
	for strings.HasPrefix(trimmed, "<@") {
		_, rest, found := strings.Cut(trimmed, ">")
		if !found {
			break
		}
		trimmed = strings.TrimSpace(rest)
	}
	if trimmed == "" {
		return "Slack thread"
	}
	runes := []rune(trimmed)
	if len(runes) > 120 {
		return string(runes[:120])
	}
	return trimmed
}

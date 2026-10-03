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

const (
	SignatureHeader = "X-Slack-Signature"
	TimestampHeader = "X-Slack-Request-Timestamp"
)

const signatureVersion = "v0"

const ReplayWindow = 5 * time.Minute

var (
	ErrNotSigned     = errors.New("slack: the request carries no signature")
	ErrBadSignature  = errors.New("slack: the signature does not match the body")
	ErrStale         = errors.New("slack: the request timestamp is outside the replay window")
	ErrNotUnderstood = errors.New("slack: the body is not an events payload")
)

func Verify(signingSecret string, header, timestamp string, body []byte, now time.Time) error {
	// Slack signs the original bytes; decoding or re-encoding before verification changes the
	// signed message. Timestamp validation and signature verification form one replay check.
	if signingSecret == "" {
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
	mac.Write([]byte(signatureVersion + ":" + timestamp + ":"))
	mac.Write(body)
	expected := signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(header)) {
		return ErrBadSignature
	}
	return nil
}

type Envelope struct {
	Challenge   string
	Application string
	Enterprise  string
	Workspace   string
	Event       Event
}

type Event struct {
	Kind        string
	Channel     string
	TS          string
	ThreadTS    string
	User        string
	Text        string
	ChannelKind string
	BotID       string
	Subtype     string
	BotUserID   string
}

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
	switch typed := decoded.EnterpriseID.(type) {
	case string:
		envelope.Enterprise = typed
	case map[string]any:
		if id, ok := typed["id"].(string); ok {
			envelope.Enterprise = id
		}
	}

	if len(decoded.Authorizations) > 0 {
		// Slack orders this block by the installation that received the event; choosing another
		// entry would resolve an enterprise event through an arbitrary workspace.
		authorized := decoded.Authorizations[0]
		if authorized.TeamID != "" {
			envelope.Workspace = authorized.TeamID
		}
		if authorized.EnterpriseID != "" {
			envelope.Enterprise = authorized.EnterpriseID
		}
		if authorized.IsBot && authorized.UserID != "" {
			envelope.Event.BotUserID = authorized.UserID
		}
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

func (e Envelope) Key() []string {
	return providerInstallationKey(e.Application, e.Enterprise, e.Workspace)
}

func providerInstallationKey(application, enterprise, workspace string) []string {
	if enterprise == "" {
		return []string{application, workspace}
	}
	return []string{application, enterprise, workspace}
}

func (e Envelope) AddressedToUs(agent string) bool {
	event := e.Event
	switch {
	case event.BotID != "":
		return false
	case agent != "" && event.User == agent:
		return false
	case event.User == "":
		return false
	case event.Subtype != "":
		return false
	case strings.TrimSpace(event.Text) == "":
		return false
	}

	switch event.Kind {
	case "app_mention":
		return true
	default:
		return false
	}
}

func (e Envelope) Thread() string {
	if e.Event.ThreadTS != "" {
		return e.Event.ThreadTS
	}
	return e.Event.TS
}

func Subject(text string) string {
	trimmed := strings.TrimSpace(text)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
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

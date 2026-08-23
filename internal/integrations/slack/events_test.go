package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// AUTHENTICITY, AND WHAT COUNTS AS SOMEBODY SPEAKING TO US.
//
// What is asserted here is what an ATTACKER would observe, because that is what these checks
// exist for: a body mutated after signing, a wrong secret, a missing signature and a stale
// timestamp are each refused, and none of them is refused in a way that says which.

// Wordy on purpose, and never a hex-looking literal. Nothing here parses the secret — it is
// HMAC key material and any bytes work — so a fixture that RESEMBLES a credential buys
// nothing and costs a red secret-scanning gate, which is how a real leak comes to be ignored.
const testSigningSecret = "a-signing-secret-that-is-not-one"

func signed(t *testing.T, secret string, at time.Time, body string) (string, string) {
	t.Helper()

	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	return "v0=" + hex.EncodeToString(mac.Sum(nil)), stamp
}

func TestACorrectSignatureOverTheExactBodyIsAccepted(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := `{"type":"event_callback","team_id":"T0ACME"}`
	signature, stamp := signed(t, testSigningSecret, now, body)

	if err := Verify(testSigningSecret, signature, stamp, []byte(body), now); err != nil {
		t.Errorf("a correctly signed request was refused: %v", err)
	}
}

func TestABodyMutatedAfterSigningIsRefused(t *testing.T) {
	t.Parallel()

	// The failure the whole check exists for. The signature is real, the timestamp is
	// fresh, and one character of the body changed on the way — which is exactly what an
	// attacker in the middle would produce.
	now := time.Now()
	body := `{"type":"event_callback","team_id":"T0ACME"}`
	signature, stamp := signed(t, testSigningSecret, now, body)

	mutated := strings.Replace(body, "T0ACME", "T0OTHER", 1)
	if err := Verify(testSigningSecret, signature, stamp, []byte(mutated), now); !errors.Is(
		err, ErrBadSignature) {
		t.Errorf("a mutated body = %v, want a refused signature", err)
	}
}

func TestAWrongSecretIsRefused(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := `{"type":"event_callback"}`
	signature, stamp := signed(t, "somebody-elses-signing-secret", now, body)

	if err := Verify(testSigningSecret, signature, stamp, []byte(body), now); !errors.Is(
		err, ErrBadSignature) {
		t.Errorf("a foreign signature = %v, want refused", err)
	}
}

func TestAnUnsignedRequestIsRefused(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := []byte(`{"type":"event_callback"}`)
	signature, stamp := signed(t, testSigningSecret, now, string(body))

	cases := map[string]struct{ signature, stamp string }{
		"no signature":                     {"", stamp},
		"no timestamp":                     {signature, ""},
		"neither":                          {"", ""},
		"a timestamp that is not a number": {signature, "the day before yesterday"},
	}
	for name, missing := range cases {
		if err := Verify(testSigningSecret, missing.signature, missing.stamp,
			body, now); !errors.Is(err, ErrNotSigned) {
			t.Errorf("%s = %v, want refused as unsigned", name, err)
		}
	}
}

func TestADeploymentWithNoSigningSecretVerifiesNothing(t *testing.T) {
	t.Parallel()

	// The second lock. Such a deployment serves no events endpoint at all, and if one were
	// ever reached it must refuse rather than accept everything — an empty secret is a
	// perfectly usable HMAC key, so "no secret" has to be a decision rather than a value.
	now := time.Now()
	body := `{"type":"event_callback"}`
	signature, stamp := signed(t, "", now, body)

	if err := Verify("", signature, stamp, []byte(body), now); !errors.Is(err, ErrNotSigned) {
		t.Errorf("an unconfigured deployment = %v, want refused", err)
	}
}

func TestATimestampOutsideTheWindowIsRefusedInBothDirections(t *testing.T) {
	t.Parallel()

	now := time.Now()
	body := `{"type":"event_callback"}`

	// A captured request replayed later, and one stamped in the future. The second is the
	// one worth having: accepting it would widen the replay window by however far ahead
	// somebody chose to stamp it.
	for name, at := range map[string]time.Time{
		"captured and replayed later": now.Add(-ReplayWindow - time.Minute),
		"stamped in the future":       now.Add(ReplayWindow + time.Minute),
	} {
		signature, stamp := signed(t, testSigningSecret, at, body)
		if err := Verify(testSigningSecret, signature, stamp, []byte(body), now); !errors.Is(
			err, ErrStale) {
			t.Errorf("%s = %v, want refused as stale", name, err)
		}
	}

	// And a request just inside the window is still accepted, or the check would be a
	// clock-skew outage rather than a replay defence.
	signature, stamp := signed(t, testSigningSecret, now.Add(-ReplayWindow+time.Second), body)
	if err := Verify(testSigningSecret, signature, stamp, []byte(body), now); err != nil {
		t.Errorf("a request inside the window was refused: %v", err)
	}
}

func TestTheURLVerificationChallengeIsReadAndCarriesNoEvent(t *testing.T) {
	t.Parallel()

	envelope, err := Parse([]byte(
		`{"type":"url_verification","challenge":"3eZbrw1a","token":"legacy"}`))
	if err != nil {
		t.Fatalf("parsing a challenge: %v", err)
	}
	if envelope.Challenge != "3eZbrw1a" {
		t.Errorf("challenge = %q", envelope.Challenge)
	}
	if envelope.Event.Kind != "" {
		t.Errorf("a challenge carried an event: %+v", envelope.Event)
	}
}

func TestAnEnvelopeNamesTheInstallationItResolvesThrough(t *testing.T) {
	t.Parallel()

	envelope, err := Parse([]byte(`{
		"type":"event_callback",
		"api_app_id":"A0OPENCLUSTER",
		"team_id":"T0ACME",
		"event":{"type":"app_mention","channel":"C1","ts":"1.1","user":"U9","text":"hello"}
	}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	key := envelope.Key()
	if key.Application != "A0OPENCLUSTER" || key.Workspace != "T0ACME" {
		t.Errorf("key = %+v", key)
	}
}

func TestAGridInstallResolvesThroughItsEnterpriseAndWorkspace(t *testing.T) {
	t.Parallel()

	// The authorizations block is what Slack says the event was delivered to, and on a
	// grid install the top-level team can be the enterprise rather than the workspace.
	envelope, err := Parse([]byte(`{
		"type":"event_callback",
		"api_app_id":"A0OPENCLUSTER",
		"team_id":"E0GRID",
		"enterprise_id":"E0GRID",
		"authorizations":[{"enterprise_id":"E0GRID","team_id":"T0DIVISION",
		                   "user_id":"U0BOT","is_bot":true}],
		"event":{"type":"app_mention","channel":"C1","ts":"1.1","user":"U9","text":"hi"}
	}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	key := envelope.Key()
	if key.Enterprise != "E0GRID" || key.Workspace != "T0DIVISION" {
		t.Errorf("key = %+v, want the division's workspace under the grid", key)
	}
}

func TestABodyThatIsNotAnEventsPayloadIsRefused(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"not json":              `{"type":`,
		"a challenge with none": `{"type":"url_verification"}`,
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrNotUnderstood) {
			t.Errorf("%s = %v, want refused", name, err)
		}
	}
}

func TestOurOwnMessageIsDiscardedBeforeAnythingElse(t *testing.T) {
	t.Parallel()

	// Not a nicety. An agent that answers its own message in a thread answers its answer,
	// and keeps going until a rate limit stops it.
	envelope := Envelope{Event: Event{
		Kind: "app_mention", Channel: "C1", TS: "1.1", User: "U0BOT", Text: "the answer",
	}}
	if envelope.AddressedToUs("U0BOT") {
		t.Error("our own message reads as somebody speaking to us")
	}
	// And the same message from anybody else is.
	envelope.Event.User = "U9HUMAN"
	if !envelope.AddressedToUs("U0BOT") {
		t.Error("a person's mention does not read as addressed to us")
	}
}

func TestWhatIsNotSomebodySpeakingToUsIsDiscarded(t *testing.T) {
	t.Parallel()

	base := Event{Kind: "app_mention", Channel: "C1", TS: "1.1", User: "U9", Text: "hello"}
	cases := map[string]Event{
		"another app posting": {Kind: "app_mention", Channel: "C1", TS: "1.1",
			User: "U9", Text: "hello", BotID: "B123"},
		"an edit or a join": {Kind: "message", Channel: "C1", TS: "1.1", User: "U9",
			Text: "hello", ChannelKind: "im", Subtype: "message_changed"},
		"a message with no author": {Kind: "app_mention", Channel: "C1", TS: "1.1",
			Text: "hello"},
		"an empty message": {Kind: "app_mention", Channel: "C1", TS: "1.1", User: "U9",
			Text: "   "},
		"a channel message that is not a mention": {Kind: "message", Channel: "C1",
			TS: "1.1", User: "U9", Text: "talking to a colleague", ChannelKind: "channel"},
		"a reaction": {Kind: "reaction_added", Channel: "C1", TS: "1.1", User: "U9",
			Text: "hello"},
	}
	for name, event := range cases {
		if (Envelope{Event: event}).AddressedToUs("U0BOT") {
			t.Errorf("%s reads as somebody speaking to us", name)
		}
	}

	// A direct message to the agent IS, because in a DM there is nobody else to be talking
	// to.
	direct := base
	direct.Kind, direct.ChannelKind = "message", "im"
	if !(Envelope{Event: direct}).AddressedToUs("U0BOT") {
		t.Error("a direct message to the agent does not read as addressed to us")
	}
}

func TestAReplyBelongsToTheThreadTheQuestionWasAskedIn(t *testing.T) {
	t.Parallel()

	// A mention inside a thread continues that thread; a mention that started none is
	// keyed on its OWN timestamp, which is the thread the reply then creates. One key, one
	// path — so "start a thread" and "continue a thread" cannot disagree about which
	// conversation a message belongs to.
	inThread := Envelope{Event: Event{TS: "200.2", ThreadTS: "100.1"}}
	if inThread.Thread() != "100.1" {
		t.Errorf("a threaded mention keys on %q, want the thread", inThread.Thread())
	}
	fresh := Envelope{Event: Event{TS: "200.2"}}
	if fresh.Thread() != "200.2" {
		t.Errorf("a new mention keys on %q, want its own timestamp", fresh.Thread())
	}
}

func TestASubjectIsTheQuestionRatherThanItsMarkup(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"<@U0BOT> why is checkout failing?": "why is checkout failing?",
		"<@U0BOT> <@U9> first line\nsecond": "first line",
		"   ":                               "Slack thread",
		"<@U0BOT>":                          "Slack thread",
	}
	for text, want := range cases {
		if got := Subject(text); got != want {
			t.Errorf("Subject(%q) = %q, want %q", text, got, want)
		}
	}

	// Bounded, because it goes in a column and because a subject is a label rather than a
	// transcript.
	if got := Subject(strings.Repeat("x", 500)); len([]rune(got)) != 120 {
		t.Errorf("a long first line produced a %d-rune subject", len([]rune(got)))
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
)

// SOMEBODY SPEAKING TO OPENCLUSTER IN SLACK, AT THE COMPOSITION SEAM.
//
// The whole flow runs: a real intake listener, a real database, the real installation
// resolution, and a workspace signing its requests the way Slack does. What is asserted is
// what a customer or an attacker would observe — the HTTP answer, and what ended up in the
// database — never how any of it is implemented.
//
// The refusals matter as much as the acceptance. A forged request, a body changed after
// signing, a stale one and a workspace this deployment does not know are each turned away,
// and they are turned away INDISTINGUISHABLY: a caller who could tell "your signature was
// fine and I do not know that workspace" from "your signature was wrong" has learned that
// their signing secret is correct.

const slackSigningSecret = "the-slack-signing-secret"

// slackEventPlane is a control plane with intake bound and the Slack agent live.
type slackEventPlane struct {
	*integrationPlane
	intake string
	dsn    string
}

func startSlackEventPlane(t *testing.T, vendor *vendorFake, live bool) *slackEventPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	intakeAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		cfg.SlackAPIURL = vendor.URL
		cfg.SlackClientID = "4444.5555"
		cfg.SlackClientSecret = "the-slack-client-secret"
		cfg.SlackSigningSecret = slackSigningSecret
		cfg.OperatorPublicURL = "http://" + operatorAddress
		if live {
			// The staged rollout gate, which is deployment configuration and not tenant
			// policy: it says "we are not offering this yet", and it is deleted when the
			// surface is generally available.
			cfg.SlackAgentOrganizations = []string{surfaceOrg}
		}
		dsn = cfg.Placements["shared"]
	})
	return &slackEventPlane{
		integrationPlane: &integrationPlane{controlPlane: plane, operator: operatorAddress},
		intake:           intakeAddress,
		dsn:              dsn,
	}
}

// deliverEvent signs a body the way Slack does and posts it to the events endpoint.
func (p *slackEventPlane) deliverEvent(t *testing.T, body string) (int, string) {
	t.Helper()
	return p.deliverEventSignedAt(t, body, body, time.Now())
}

// deliverEventSignedAt signs one body and sends another, so a test can mutate a payload
// after it was signed — which is exactly what an attacker in the middle does.
func (p *slackEventPlane) deliverEventSignedAt(
	t *testing.T, signedBody, sentBody string, at time.Time,
) (int, string) {
	t.Helper()

	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(slackSigningSecret))
	mac.Write([]byte("v0:" + stamp + ":" + signedBody))
	signature := "v0=" + hexOf(mac.Sum(nil))

	return p.postEvent(t, sentBody, map[string]string{
		slack.SignatureHeader: signature,
		slack.TimestampHeader: stamp,
	})
}

func (p *slackEventPlane) postEvent(
	t *testing.T, body string, headers map[string]string,
) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+p.intake+intake.SlackEventsPath, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering the event: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return response.StatusCode, string(answer)
}

func hexOf(sum []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, 0, len(sum)*2)
	for _, b := range sum {
		encoded = append(encoded, digits[b>>4], digits[b&0x0f])
	}
	return string(encoded)
}

// mention is an @OpenCluster in a channel, as Slack delivers one.
func mention(text, channel, ts, threadTS, user string) string {
	thread := ""
	if threadTS != "" {
		thread = `"thread_ts":"` + threadTS + `",`
	}
	return `{"type":"event_callback","api_app_id":"A0OPENCLUSTER","team_id":"T0ACME",` +
		`"event_id":"Ev` + ts + `","event":{"type":"app_mention","channel":"` + channel +
		`","ts":"` + ts + `",` + thread + `"user":"` + user + `","text":"` + text + `"}}`
}

// conversationsIn reports what the tenant's conversations look like in the database, which
// is the honest place to assert a surface nothing else exposes yet.
func (p *slackEventPlane) conversationsIn(t *testing.T) []struct {
	ID       string
	Surface  int16
	Subject  string
	Messages int
} {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	rows, err := database.Query(ctx, `
		SELECT c.conversation_id, c.surface, c.subject,
		       (SELECT count(*) FROM conversation_message m
		         WHERE m.org_id = c.org_id AND m.conversation_id = c.conversation_id)
		  FROM conversation c
		 WHERE c.org_id = $1
		 ORDER BY c.created_at`, surfaceOrg)
	if err != nil {
		t.Fatalf("reading conversations: %v", err)
	}
	defer rows.Close()

	var found []struct {
		ID       string
		Surface  int16
		Subject  string
		Messages int
	}
	for rows.Next() {
		var one struct {
			ID       string
			Surface  int16
			Subject  string
			Messages int
		}
		if err := rows.Scan(&one.ID, &one.Surface, &one.Subject, &one.Messages); err != nil {
			t.Fatalf("scanning a conversation: %v", err)
		}
		found = append(found, one)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading conversations: %v", err)
	}
	return found
}

// connectWorkspace drives the one-click flow so the deployment has an installation to route
// events through. Without it there is nothing for a workspace to resolve to.
func (p *slackEventPlane) connectWorkspace(t *testing.T) {
	t.Helper()

	status, landed := connectSlack(t, p.integrationPlane, "the-authorization-code")
	if status != http.StatusOK {
		t.Fatalf("connecting the workspace = %d: %s", status, landed)
	}
}

// The sentence this slice exists for.
func TestSlackEvents_AMentionOpensAConversationInItsThread(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	status, answer := plane.deliverEvent(t,
		mention("<@U0BOT> why is checkout failing?", "C0INCIDENTS", "1700000001.1", "", "U9SRE"))
	if status != http.StatusOK {
		t.Fatalf("a mention answered %d: %s", status, answer)
	}

	found := plane.conversationsIn(t)
	if len(found) != 1 {
		t.Fatalf("a mention produced %d conversations, want one: %+v", len(found), found)
	}
	if found[0].Surface != 2 {
		t.Errorf("the conversation's surface = %d, want slack", found[0].Surface)
	}
	if found[0].Subject != "why is checkout failing?" {
		t.Errorf("subject = %q; the mention markup is not the question", found[0].Subject)
	}
	if found[0].Messages != 1 {
		t.Errorf("the conversation holds %d messages, want the question", found[0].Messages)
	}
}

// A mention inside a thread continues that conversation, and a colleague replying in it is
// part of the same one — which is what makes a shared investigation genuinely shared.
func TestSlackEvents_AThreadIsOneConversationHoweverManyPeopleSpeak(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	if status, answer := plane.deliverEvent(t, mention(
		"<@U0BOT> checkout is erroring", "C0INCIDENTS", "1700000001.1", "", "U9SRE",
	)); status != http.StatusOK {
		t.Fatalf("the first mention answered %d: %s", status, answer)
	}
	if status, answer := plane.deliverEvent(t, mention(
		"<@U0BOT> only in eu-west?", "C0INCIDENTS", "1700000002.2", "1700000001.1", "U9SRE",
	)); status != http.StatusOK {
		t.Fatalf("a follow-up answered %d: %s", status, answer)
	}
	if status, answer := plane.deliverEvent(t, mention(
		"<@U0BOT> I see it too", "C0INCIDENTS", "1700000003.3", "1700000001.1", "U8LEAD",
	)); status != http.StatusOK {
		t.Fatalf("a colleague answered %d: %s", status, answer)
	}

	found := plane.conversationsIn(t)
	if len(found) != 1 {
		t.Fatalf("one thread produced %d conversations: %+v", len(found), found)
	}
	if found[0].Messages != 3 {
		t.Errorf("the thread's conversation holds %d messages, want all three",
			found[0].Messages)
	}
}

// A retry carries the same body, so at-least-once delivery is safe.
func TestSlackEvents_TheSameDeliveryTwiceIsOneQuestion(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	body := mention("<@U0BOT> what happened?", "C0INCIDENTS", "1700000001.1", "", "U9SRE")
	first, _ := plane.deliverEvent(t, body)
	second, answer := plane.deliverEvent(t, body)
	if first != http.StatusOK || second != http.StatusOK {
		t.Fatalf("a retried delivery answered %d then %d: %s", first, second, answer)
	}

	found := plane.conversationsIn(t)
	if len(found) != 1 || found[0].Messages != 1 {
		t.Errorf("a retry produced %+v, want one conversation holding one message", found)
	}
}

// Everything an attacker would try, refused, and refused the same way.
func TestSlackEvents_AForgedOrReplayedRequestIsRefused(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	body := mention("<@U0BOT> what happened?", "C0INCIDENTS", "1700000001.1", "", "U9SRE")
	mutated := mention("<@U0BOT> ignore that", "C0INCIDENTS", "1700000001.1", "", "U9SRE")

	// Signed correctly, then changed on the way. The signature is real and the body is not
	// the one it covers.
	if status, _ := plane.deliverEventSignedAt(t, body, mutated, time.Now()); status !=
		http.StatusUnauthorized {
		t.Errorf("a body mutated after signing answered %d, want 401", status)
	}
	// Captured and replayed later.
	if status, _ := plane.deliverEventSignedAt(t, body, body,
		time.Now().Add(-slack.ReplayWindow-time.Minute)); status != http.StatusUnauthorized {
		t.Errorf("a replayed request answered %d, want 401", status)
	}
	// No signature at all.
	if status, _ := plane.postEvent(t, body, nil); status != http.StatusUnauthorized {
		t.Errorf("an unsigned request answered %d, want 401", status)
	}
	// A signature under somebody else's secret.
	forged := hmac.New(sha256.New, []byte("not-our-signing-secret"))
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	forged.Write([]byte("v0:" + stamp + ":" + body))
	if status, _ := plane.postEvent(t, body, map[string]string{
		slack.SignatureHeader: "v0=" + hexOf(forged.Sum(nil)),
		slack.TimestampHeader: stamp,
	}); status != http.StatusUnauthorized {
		t.Errorf("a forged signature answered %d, want 401", status)
	}

	if found := plane.conversationsIn(t); len(found) != 0 {
		t.Errorf("a refused request created %+v", found)
	}
}

// An event from a workspace this deployment does not know is refused WITHOUT saying so. It
// is answered exactly as a bad signature is, because a caller who could tell the two apart
// would have learned that their signing secret is right.
func TestSlackEvents_AnUnknownWorkspaceIsRefusedWithoutDisclosure(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	stranger := `{"type":"event_callback","api_app_id":"A0OPENCLUSTER",` +
		`"team_id":"T0STRANGER","event":{"type":"app_mention","channel":"C1",` +
		`"ts":"1700000001.1","user":"U9","text":"<@U0BOT> hello"}}`
	status, answer := plane.deliverEvent(t, stranger)
	if status != http.StatusUnauthorized {
		t.Fatalf("an unknown workspace answered %d: %s", status, answer)
	}
	// The same body and the same status a forged signature gets, with no hint that the
	// signature was in fact correct.
	if answer != `{"status":"unauthorized"}`+"\n" {
		t.Errorf("an unknown workspace answered %q, which differs from a refused signature",
			answer)
	}
	if found := plane.conversationsIn(t); len(found) != 0 {
		t.Errorf("an unknown workspace created %+v", found)
	}
}

// The challenge Slack sends when the request URL is saved. It is answered and creates
// nothing, and it must work before any installation exists.
func TestSlackEvents_TheURLVerificationChallengeIsAnsweredAndCreatesNothing(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	plane := startSlackEventPlane(t, vendor, true)

	status, answer := plane.deliverEvent(t,
		`{"type":"url_verification","challenge":"3eZbrw1aB2Cc3","token":"legacy"}`)
	if status != http.StatusOK {
		t.Fatalf("the challenge answered %d: %s", status, answer)
	}
	var echoed struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(answer), &echoed); err != nil {
		t.Fatalf("decoding the challenge answer: %v\nbody: %s", err, answer)
	}
	if echoed.Challenge != "3eZbrw1aB2Cc3" {
		t.Errorf("the challenge echoed %q", echoed.Challenge)
	}
	if found := plane.conversationsIn(t); len(found) != 0 {
		t.Errorf("the challenge created %+v", found)
	}
}

// Our own message is discarded before anything else. An agent that answers its own message
// in a thread answers its answer, and keeps going until a rate limit stops it.
func TestSlackEvents_OpenClusterDoesNotAnswerItself(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	// The bot's own user id, which the installation recorded when the workspace was
	// connected.
	status, answer := plane.deliverEvent(t,
		mention("<@U9SRE> here is what I found", "C0INCIDENTS", "1700000009.9", "", "U0BOT"))
	if status != http.StatusOK {
		t.Fatalf("our own message answered %d: %s", status, answer)
	}
	if found := plane.conversationsIn(t); len(found) != 0 {
		t.Errorf("our own message opened %+v", found)
	}
}

// Outside the staged rollout an event is acknowledged and dropped. Acknowledged, because
// anything Slack is not told succeeded is retried, and a storm of retries for something this
// deployment is deliberately not doing is a storm it asked for.
func TestSlackEvents_OutsideTheRolloutAnEventIsAcknowledgedAndDropped(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, false)
	plane.connectWorkspace(t)

	status, answer := plane.deliverEvent(t,
		mention("<@U0BOT> are you there?", "C0INCIDENTS", "1700000001.1", "", "U9SRE"))
	if status != http.StatusOK {
		t.Fatalf("an event outside the rollout answered %d: %s", status, answer)
	}
	if found := plane.conversationsIn(t); len(found) != 0 {
		t.Errorf("an organization outside the rollout got %+v", found)
	}
}

// A deployment that holds no signing secret does not serve the endpoint at all. An endpoint
// that exists and refuses is a configuration to check; one that does not exist is a
// deployment nobody asked to receive events.
func TestSlackEvents_ADeploymentWithNoSigningSecretServesNoEndpoint(t *testing.T) {
	intakeAddress := freeAddress(t)
	startControlPlane(t, func(cfg *config.Config) {
		cfg.IntakeAddress = intakeAddress
		cfg.SlackSigningSecret = ""
	})

	plane := &slackEventPlane{intake: intakeAddress}
	status, _ := plane.postEvent(t, `{"type":"url_verification","challenge":"x"}`, nil)
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Errorf("an unconfigured deployment answered %d, want the route not to exist", status)
	}
}

// Acknowledgement stays well inside Slack's three-second timeout, asserted directly rather
// than reasoned about. Nothing on this path waits on a model, a repository or an
// investigation, and this is what says so.
func TestSlackEvents_AcknowledgementIsFastEnoughForSlack(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-installed-token")
	vendor.grant("channels:read,channels:history,users:read")
	plane := startSlackEventPlane(t, vendor, true)
	plane.connectWorkspace(t)

	began := time.Now()
	status, answer := plane.deliverEvent(t,
		mention("<@U0BOT> what happened?", "C0INCIDENTS", "1700000001.1", "", "U9SRE"))
	took := time.Since(began)
	if status != http.StatusOK {
		t.Fatalf("a mention answered %d: %s", status, answer)
	}
	// Slack's own timeout is three seconds. The margin is deliberate: this must not be a
	// test that starts failing on a loaded CI runner before the product starts failing for
	// a customer.
	if took > 2*time.Second {
		t.Errorf("acknowledgement took %s, which is not comfortably inside Slack's timeout",
			took)
	}
}

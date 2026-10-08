package controlplane

import (
	"context"
	"crypto/sha256"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackMessageRecoveryRequiresAnAgentBeforeMutation(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-terminal-no-agent")
	plane := startSlackPlaneWithOptions(t, vendor, app.Options{})
	status, body := plane.createSlack(t, "Terminal Slack source", "xoxb-terminal-no-agent")
	if status != http.StatusCreated {
		t.Fatalf("creating Slack Integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)

	database, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	conversationID := recordTerminalSlackMessageWork(t, database, created.Integration.ID,
		"terminal-without-agent", []byte(`{"event_id":"terminal-without-agent"}`), 8)

	type recoveryState struct {
		Status         int
		Attempts       int
		LeaseOwner     string
		LeaseEpoch     int64
		LeaseExpiresAt *time.Time
		AvailableAt    time.Time
		FailureClass   string
		FailureMessage string
		UpdatedAt      time.Time
		SuccessAudits  int
	}
	readState := func() recoveryState {
		t.Helper()
		var state recoveryState
		err := database.QueryRow(context.Background(), `SELECT status, attempts, lease_owner,
			lease_epoch, lease_expires_at, available_at, failure_class, failure_message, updated_at,
			(SELECT count(*) FROM audit_event
			  WHERE org_id = work.org_id AND action = 'slack-message.recovered')
			FROM slack_message_work work
			WHERE org_id = $1 AND conversation_id = $2 AND message_sequence = 1`,
			surfaceOrg, conversationID).Scan(&state.Status, &state.Attempts, &state.LeaseOwner,
			&state.LeaseEpoch, &state.LeaseExpiresAt, &state.AvailableAt, &state.FailureClass,
			&state.FailureMessage, &state.UpdatedAt, &state.SuccessAudits)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readState()

	status, body = plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/slack/conversations/"+conversationID+"/messages/1/recover", nil)
	if status != http.StatusServiceUnavailable ||
		!strings.Contains(body, "no model provider configured") {
		t.Fatalf("recovering Slack Message without an Agent = %d: %s", status, body)
	}
	after := readState()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("refused Slack recovery changed work state: before=%+v after=%+v", before, after)
	}
}

func TestSlackMessageWorkWaitsForAnAgentAndProcessesAfterRestart(t *testing.T) {
	vendor := newVendorFake(t, "xoxb-durable-question")
	plane := startSlackPlaneWithOptions(t, vendor, app.Options{})
	status, body := plane.createSlack(t, "Durable Slack source", "xoxb-durable-question")
	if status != http.StatusCreated {
		t.Fatalf("creating Slack Integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	integrationID, err := uuid.Parse(created.Integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	database := openDatabase(t, plane.dsn)
	digest := sha256.Sum256([]byte("durable accepted Slack question"))
	outcome, err := database.RecordSlackMessage(context.Background(), uuid.MustParse(surfaceOrg), storage.SlackMessage{
		Integration:   integrationID,
		ContentDigest: digest[:],
		Channel:       "C0DURABLE",
		Thread:        "1700000100.1",
		MessageID:     "1700000100.1",
		Subject:       "durable question",
		ActorID:       "U9SRE",
		ActorDisplay:  "SRE",
		Text:          "what happened?",
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(750 * time.Millisecond)
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var workStatus, attempts int
	var leaseOwner string
	var leaseEpoch int64
	if err = connection.QueryRow(context.Background(), `SELECT status, attempts, lease_owner, lease_epoch
		FROM slack_message_work WHERE org_id = $1 AND conversation_id = $2`,
		surfaceOrg, outcome.Conversation).Scan(&workStatus, &attempts, &leaseOwner, &leaseEpoch); err != nil {
		t.Fatal(err)
	}
	detail, err := database.ConversationDetail(context.Background(), uuid.MustParse(surfaceOrg), outcome.Conversation, 50)
	if err != nil {
		t.Fatal(err)
	}
	if workStatus != int(storage.SlackMessageReady) || attempts != 0 || leaseOwner != "" ||
		leaseEpoch != 0 || len(detail.Messages) != 1 || len(detail.Turns) != 0 {
		t.Fatalf("no-Agent process touched accepted Slack work: status=%d attempts=%d owner=%q epoch=%d detail=%+v",
			workStatus, attempts, leaseOwner, leaseEpoch, detail)
	}

	plane.shutdown()
	startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.HTTPListenAddress = freeAddress(t)
		cfg.InvestigationWorkers = 1
	}, app.Options{Agent: &blockingAgentMain{}, SlackAPIURL: vendor.URL})

	deadline := time.Now().Add(10 * time.Second)
	for {
		detail, err = database.ConversationDetail(context.Background(), uuid.MustParse(surfaceOrg), outcome.Conversation, 50)
		if err == nil && len(detail.Messages) == 1 && len(detail.Turns) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted Slack question was not processed after Agent restart: %+v, %v", detail, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err = connection.QueryRow(context.Background(), `SELECT status, attempts
		FROM slack_message_work WHERE org_id = $1 AND conversation_id = $2`,
		surfaceOrg, outcome.Conversation).Scan(&workStatus, &attempts); err != nil {
		t.Fatal(err)
	}
	if workStatus != int(storage.SlackMessageComplete) || attempts != 1 {
		t.Fatalf("Agent restart processed Slack work status=%d attempts=%d", workStatus, attempts)
	}
}

func TestConversationFollowupRefusalPreservesActiveAndQueuedWork(t *testing.T) {
	agent := &blockingAgentMain{started: make(chan uuid.UUID, 1)}
	plane, _ := agentPlane(t, agent)
	conversationID, _ := plane.openConversation(t, "durable queued Conversation", "first question")
	select {
	case <-agent.started:
	case <-time.After(10 * time.Second):
		t.Fatal("initial Investigation was not claimed")
	}
	path := plane.base(surfaceOrg) + "/conversations/" + conversationID
	status, body := plane.call(t, http.MethodPost, path+"/messages",
		map[string]any{"message": "queued follow-up"})
	if status != http.StatusAccepted || !strings.Contains(body, `"queued":true`) {
		t.Fatalf("queueing follow-up before downgrade = %d: %s", status, body)
	}
	plane.shutdown()

	apiAddress := freeAddress(t)
	restarted := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.HTTPListenAddress = apiAddress
		cfg.MaxPendingInvestigations = 1
	}, app.Options{})
	restarted.sessionCookie = plane.sessionCookie
	withoutAgent := &integrationPlane{controlPlane: restarted, api: apiAddress, dsn: plane.dsn}
	restartedPath := withoutAgent.base(surfaceOrg) + "/conversations/" + conversationID

	database, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	snapshot := func() string {
		t.Helper()
		var state string
		err := database.QueryRow(context.Background(), `SELECT jsonb_build_object(
			'conversation', (SELECT to_jsonb(c) FROM conversation c
			 WHERE c.org_id = $1 AND c.conversation_id = $2),
			'messages', (SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.sequence), '[]'::jsonb)
			 FROM conversation_message m WHERE m.org_id = $1 AND m.conversation_id = $2),
			'investigations', (SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.created_at, i.investigation_id), '[]'::jsonb)
			 FROM investigation i WHERE i.org_id = $1 AND i.conversation_id = $2),
			'success_audits', (SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.occurred_at, a.event_id), '[]'::jsonb)
			 FROM audit_event a WHERE a.org_id = $1
			 AND a.action IN ('conversation.opened', 'conversation.message-sent', 'investigation.opened'))
		)::text`, surfaceOrg, conversationID).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	status, beforeRead := withoutAgent.call(t, http.MethodGet, restartedPath, nil)
	if status != http.StatusOK {
		t.Fatalf("reading retained work before refusal = %d: %s", status, beforeRead)
	}

	status, body = withoutAgent.call(t, http.MethodPost, restartedPath+"/messages",
		map[string]any{"message": "must not be accepted"})
	if status != http.StatusServiceUnavailable ||
		!strings.Contains(body, "no model provider configured") {
		t.Fatalf("following up after Agent downgrade = %d: %s", status, body)
	}
	after := snapshot()
	status, afterRead := withoutAgent.call(t, http.MethodGet, restartedPath, nil)
	if status != http.StatusOK {
		t.Fatalf("reading retained work after refusal = %d: %s", status, afterRead)
	}
	if after != before || afterRead != beforeRead {
		t.Fatalf("refused follow-up changed retained work\nbefore database: %s\nafter database: %s\nbefore API: %s\nafter API: %s",
			before, after, beforeRead, afterRead)
	}
}

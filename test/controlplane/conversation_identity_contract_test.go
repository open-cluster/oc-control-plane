package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestWebConversationExposesTrustedSourceAndUserAttribution(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Completer: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}

	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "trusted attribution", "message": "what changed?"})
	if status != http.StatusCreated {
		t.Fatalf("opening Conversation = %d: %s", status, body)
	}
	var opened map[string]json.RawMessage
	decodeInto(t, body, &opened)
	var id string
	if err := json.Unmarshal(opened["id"], &id); err != nil {
		t.Fatal(err)
	}
	if string(opened["source"]) != `"web"` {
		t.Errorf("source = %s, want web", opened["source"])
	}
	if _, present := opened["surface"]; present {
		t.Errorf("retired surface was exposed: %s", body)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(opened["message"], &message); err != nil {
		t.Fatal(err)
	}
	if string(message["role"]) != `"user"` {
		t.Errorf("role = %s, want user", message["role"])
	}
	if _, present := message["actorKind"]; present {
		t.Errorf("retired actorKind was exposed: %s", body)
	}
	if string(message["actorId"]) == `""` || string(message["actorDisplay"]) == `""` {
		t.Errorf("trusted attribution was not retained: %s", body)
	}

	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations", nil)
	if status != http.StatusOK {
		t.Fatalf("listing Conversations = %d: %s", status, body)
	}
	var listed struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	decodeInto(t, body, &listed)
	if len(listed.Items) != 1 || string(listed.Items[0]["source"]) != `"web"` {
		t.Fatalf("listed Conversation identity = %s", body)
	}

	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("reading Conversation = %d: %s", status, body)
	}
	var detail struct {
		Source   string                       `json:"source"`
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	decodeInto(t, body, &detail)
	if detail.Source != "web" || len(detail.Messages) != 1 || string(detail.Messages[0]["role"]) != `"user"` {
		t.Fatalf("Conversation detail identity = %s", body)
	}
}

func TestConversationAIAdmissionRequiresAnAgentBeforeDurableMutation(t *testing.T) {
	apiAddress := freeAddress(t)
	var dsn string
	controlPlane := startControlPlane(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		dsn = cfg.DatabaseDSN
	})
	plane := &integrationPlane{controlPlane: controlPlane, api: apiAddress}

	status, body := plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "unavailable execution", "message": "what happened?"})
	if status != http.StatusServiceUnavailable ||
		!strings.Contains(body, "no model provider configured") {
		t.Fatalf("opening Conversation without an Agent = %d: %s", status, body)
	}

	database, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	var conversations, messages, investigations, successAudits int
	err = database.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM conversation WHERE org_id = $1),
		(SELECT count(*) FROM conversation_message WHERE org_id = $1),
		(SELECT count(*) FROM investigation WHERE org_id = $1),
		(SELECT count(*) FROM audit_event
		  WHERE org_id = $1 AND action IN ('conversation.opened', 'conversation.message-sent'))`,
		surfaceOrg).Scan(&conversations, &messages, &investigations, &successAudits)
	if err != nil {
		t.Fatal(err)
	}
	if conversations != 0 || messages != 0 || investigations != 0 || successAudits != 0 {
		t.Fatalf("refused initial Message persisted Conversations=%d Messages=%d Investigations=%d success audits=%d",
			conversations, messages, investigations, successAudits)
	}

	base := plane.base(surfaceOrg) + "/conversations"

	status, body = plane.call(t, http.MethodPost, base,
		map[string]any{"subject": "retained operational history"})
	if status != http.StatusCreated {
		t.Fatalf("opening empty Conversation without an Agent = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &opened); err != nil || opened.ID == "" {
		t.Fatalf("decoding opened Conversation: %v: %s", err, body)
	}
	for _, path := range []string{base, base + "/" + opened.ID, base + "/" + opened.ID + "/turns"} {
		if status, body = plane.call(t, http.MethodGet, path, nil); status != http.StatusOK {
			t.Fatalf("reading retained Conversation at %s = %d: %s", path, status, body)
		}
	}
	var beforeActivity time.Time
	var beforeMessages, beforeInvestigations, beforeAudits int
	err = database.QueryRow(context.Background(), `SELECT c.last_activity_at,
		(SELECT count(*) FROM conversation_message WHERE org_id = c.org_id AND conversation_id = c.conversation_id),
		(SELECT count(*) FROM investigation WHERE org_id = c.org_id AND conversation_id = c.conversation_id),
		(SELECT count(*) FROM audit_event WHERE org_id = c.org_id AND action = 'conversation.message-sent')
		FROM conversation c WHERE c.org_id = $1 AND c.conversation_id = $2`,
		surfaceOrg, opened.ID).Scan(&beforeActivity, &beforeMessages, &beforeInvestigations, &beforeAudits)
	if err != nil {
		t.Fatal(err)
	}

	status, body = plane.call(t, http.MethodPost, base+"/"+opened.ID+"/messages",
		map[string]any{"message": "do not strand this"})
	if status != http.StatusServiceUnavailable ||
		!strings.Contains(body, "no model provider configured") {
		t.Fatalf("following up without an Agent = %d: %s", status, body)
	}

	var afterActivity time.Time
	var afterMessages, afterInvestigations, afterAudits int
	err = database.QueryRow(context.Background(), `SELECT c.last_activity_at,
		(SELECT count(*) FROM conversation_message WHERE org_id = c.org_id AND conversation_id = c.conversation_id),
		(SELECT count(*) FROM investigation WHERE org_id = c.org_id AND conversation_id = c.conversation_id),
		(SELECT count(*) FROM audit_event WHERE org_id = c.org_id AND action = 'conversation.message-sent')
		FROM conversation c WHERE c.org_id = $1 AND c.conversation_id = $2`,
		surfaceOrg, opened.ID).Scan(&afterActivity, &afterMessages, &afterInvestigations, &afterAudits)
	if err != nil {
		t.Fatal(err)
	}
	if !afterActivity.Equal(beforeActivity) || afterMessages != beforeMessages ||
		afterInvestigations != beforeInvestigations || afterAudits != beforeAudits {
		t.Fatalf("refused follow-up changed activity=%v Messages=%d Investigations=%d audits=%d; before activity=%v Messages=%d Investigations=%d audits=%d",
			afterActivity, afterMessages, afterInvestigations, afterAudits,
			beforeActivity, beforeMessages, beforeInvestigations, beforeAudits)
	}
}

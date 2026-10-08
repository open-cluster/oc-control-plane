package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"

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
	address := freeAddress(t)
	var dsn string
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		dsn = cfg.DatabaseDSN
	}, app.Options{})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}

	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "unavailable AI", "message": "what changed?"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("opening Conversation with a Message = %d, want 503: %s", status, body)
	}

	ctx := context.Background()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil { t.Fatal(err) }
	defer func() { _ = connection.Close(ctx) }()
	for table, query := range map[string]string{
		"conversation": "SELECT count(*) FROM conversation WHERE org_id = $1",
		"message": "SELECT count(*) FROM conversation_message WHERE org_id = $1",
		"investigation": "SELECT count(*) FROM investigation WHERE org_id = $1",
	} {
		var count int
		if err = connection.QueryRow(ctx, query, surfaceOrg).Scan(&count); err != nil { t.Fatal(err) }
		if count != 0 { t.Fatalf("%s count = %d after refused AI work, want 0", table, count) }
	}

	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "continuity only"})
	if status != http.StatusCreated { t.Fatalf("opening empty Conversation = %d: %s", status, body) }
	var opened map[string]json.RawMessage
	decodeInto(t, body, &opened)
	var id string
	if err = json.Unmarshal(opened["id"], &id); err != nil { t.Fatal(err) }

	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations/"+id+"/messages",
		map[string]any{"message": "now investigate"})
	if status != http.StatusServiceUnavailable { t.Fatalf("appending Message without Agent = %d, want 503: %s", status, body) }

	var messages, investigations int
	if err = connection.QueryRow(ctx, "SELECT count(*) FROM conversation_message WHERE org_id = $1", surfaceOrg).Scan(&messages); err != nil { t.Fatal(err) }
	if err = connection.QueryRow(ctx, "SELECT count(*) FROM investigation WHERE org_id = $1", surfaceOrg).Scan(&investigations); err != nil { t.Fatal(err) }
	if messages != 0 || investigations != 0 { t.Fatalf("refused follow-up stored messages=%d investigations=%d", messages, investigations) }
}

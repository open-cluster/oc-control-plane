package controlplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
)

func TestConversationAdmissionWithoutAgentRefusesOnlyAIWork(t *testing.T) {
	plane := startIntegrationPlaneWithOptions(t, app.Options{})
	base := plane.base(surfaceOrg) + "/conversations"

	status, body := plane.call(t, http.MethodPost, base, map[string]any{
		"subject": "checkout",
		"message": "what changed?",
	})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("Conversation with initial Message without Agent = %d: %s", status, body)
	}

	ctx := context.Background()
	connection, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()

	var conversations, messages, investigations int
	if err = connection.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM conversation WHERE org_id = $1),
		       (SELECT count(*) FROM conversation_message WHERE org_id = $1),
		       (SELECT count(*) FROM investigation WHERE org_id = $1)`,
		surfaceOrg).Scan(&conversations, &messages, &investigations); err != nil {
		t.Fatal(err)
	}
	if conversations != 0 || messages != 0 || investigations != 0 {
		t.Fatalf("refused initial Message mutated state: conversations=%d messages=%d investigations=%d",
			conversations, messages, investigations)
	}

	status, body = plane.call(t, http.MethodPost, base, map[string]any{
		"subject": "saved context",
	})
	if status != http.StatusCreated {
		t.Fatalf("empty Conversation without Agent = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	if opened.ID == "" {
		t.Fatal("empty Conversation returned no identity")
	}

	if status, body = plane.call(t, http.MethodGet, base+"/"+opened.ID, nil); status != http.StatusOK {
		t.Fatalf("reading retained Conversation without Agent = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, base+"/"+opened.ID+"/messages",
		map[string]any{"message": "now investigate"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("follow-up without Agent = %d: %s", status, body)
	}

	if err = connection.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM conversation_message
		         WHERE org_id = $1 AND conversation_id = $2),
		       (SELECT count(*) FROM investigation
		         WHERE org_id = $1 AND conversation_id = $2)`,
		surfaceOrg, opened.ID).Scan(&messages, &investigations); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || investigations != 0 {
		t.Fatalf("refused follow-up mutated state: messages=%d investigations=%d",
			messages, investigations)
	}
}

func TestInjectedAgentAllowsConversationWithoutConfiguredProvider(t *testing.T) {
	plane := startIntegrationPlaneWithOptions(t, app.Options{Agent: &blockingAgentMain{}})
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations",
		map[string]any{"subject": "injected Agent", "message": "investigate checkout"})
	if status != http.StatusCreated {
		t.Fatalf("injected Agent was treated as unavailable = %d: %s", status, body)
	}
}

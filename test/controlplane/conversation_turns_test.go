package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestConversationAndInvestigationStatusesFollowAssignmentUntilRecovery(t *testing.T) {
	ctx := context.Background()
	agent := &blockingAgentMain{started: make(chan uuid.UUID, 1)}
	plane, _ := agentPlane(t, agent)
	plane.openConversation(t, "hold the worker", "first question")
	select {
	case <-agent.started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not claim the first turn")
	}
	conversation, id := plane.openConversation(t, "assignment lifecycle", "second question")
	assertStatus := func(want string) {
		t.Helper()
		for _, path := range []string{"/investigations/" + id, "/conversations/" + conversation, "/conversations/" + conversation + "/turns"} {
			status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
			if status != http.StatusOK {
				t.Fatalf("reading %s = %d: %s", path, status, body)
			}
			var read struct {
				Status string `json:"status"`
				Turns  []struct {
					Status string `json:"status"`
				} `json:"turns"`
			}
			decodeInto(t, body, &read)
			if len(read.Turns) == 1 {
				read.Status = read.Turns[0].Status
			}
			if read.Status != want {
				t.Fatalf("%s status = %q, want %q: %s", path, read.Status, want, body)
			}
		}
	}
	assertStatus("queued")
	database, err := storage.OpenDatabase(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	org, claimed, took, err := database.ClaimInvestigation(ctx, investigation.Claim{Worker: "lifecycle-test", LeaseFor: time.Minute})
	if err != nil || !took || claimed.ID.String() != id {
		t.Fatalf("claim = %s took=%v err=%v", claimed.ID, took, err)
	}
	assertStatus("running")
	pool, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close(ctx) })
	if _, err := pool.Exec(ctx, `UPDATE investigation SET lease_expires_at = now() - interval '1 minute' WHERE org_id = $1 AND investigation_id = $2`, org, claimed.ID); err != nil {
		t.Fatal(err)
	}
	assertStatus("running")
	if n, err := database.RecoverStale(ctx, investigation.RecoveryReason, 1); err != nil || n != 1 {
		t.Fatalf("recovery = %d: %v", n, err)
	}
	assertStatus("failed")
}

func TestConversationTurnsPaginateAndRejectInvalidQueries(t *testing.T) {
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "zai", "glm-4.7", "scripted-model-key"
	}, app.Options{Completer: concludingModel{}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	id, first := plane.openConversation(t, "paginated conversation", "first question")
	plane.awaitInvestigation(t, first)
	path := plane.base(surfaceOrg) + "/conversations/" + id
	status, body := plane.call(t, http.MethodPost, path+"/messages", map[string]any{"message": "second question"})
	if status != http.StatusAccepted {
		t.Fatalf("follow-up = %d: %s", status, body)
	}
	var accepted struct {
		Turn struct{ InvestigationID string } `json:"turn"`
	}
	decodeInto(t, body, &accepted)
	plane.awaitInvestigation(t, accepted.Turn.InvestigationID)
	status, body = plane.call(t, http.MethodGet, path+"/turns?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("first page = %d: %s", status, body)
	}
	var page struct {
		Turns []struct {
			InvestigationID string `json:"investigationId"`
			Turn            int    `json:"turn"`
		} `json:"turns"`
		Next *string `json:"next"`
	}
	decodeInto(t, body, &page)
	if len(page.Turns) != 1 || page.Turns[0].InvestigationID != first || page.Turns[0].Turn != 1 || page.Next == nil {
		t.Fatalf("first page = %s", body)
	}
	cursor := url.QueryEscape(*page.Next)
	status, body = plane.call(t, http.MethodGet, path+"/turns?limit=1&cursor="+cursor, nil)
	if status != http.StatusOK {
		t.Fatalf("next page = %d: %s", status, body)
	}
	decodeInto(t, body, &page)
	if len(page.Turns) != 1 || page.Turns[0].InvestigationID != accepted.Turn.InvestigationID || page.Turns[0].Turn != 2 || page.Next != nil {
		t.Fatalf("last page = %s", body)
	}
	var rawPage struct {
		Turns []map[string]json.RawMessage `json:"turns"`
	}
	decodeInto(t, body, &rawPage)
	if _, present := rawPage.Turns[0]["answer"]; present {
		t.Fatalf("Turn exposed the Conversation reply: %s", body)
	}
	status, body = plane.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("Conversation detail = %d: %s", status, body)
	}
	var detail struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	decodeInto(t, body, &detail)
	wantRoles := []string{`"user"`, `"assistant"`, `"user"`, `"assistant"`}
	if len(detail.Messages) != len(wantRoles) {
		t.Fatalf("Conversation messages = %s", body)
	}
	for index, role := range wantRoles {
		if string(detail.Messages[index]["role"]) != role {
			t.Fatalf("Message %d role = %s, want %s: %s", index+1,
				detail.Messages[index]["role"], role, body)
		}
		if role == `"assistant"` && len(detail.Messages[index]["investigationId"]) == 0 {
			t.Fatalf("assistant Message %d has no Investigation: %s", index+1, body)
		}
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=", "limit=1&limit=2", "cursor=", "cursor=bad", "cursor=a&cursor=b", "search=answer", "sort=turn", "unknown=value", "cursor=%zz", "limit=1;sort=turn"} {
		if status, body := plane.call(t, http.MethodGet, path+"/turns?"+query, nil); status != http.StatusBadRequest {
			t.Errorf("query %s = %d: %s", query, status, body)
		}
	}
	other, _ := plane.openConversation(t, "another conversation", "")
	if status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/conversations/"+other+"/turns?cursor="+cursor, nil); status != http.StatusBadRequest {
		t.Errorf("cursor reused across conversations = %d: %s", status, body)
	}
	for _, suffix := range []string{"", "/turns?limit=200"} {
		status, body := plane.call(t, http.MethodGet, path+suffix, nil)
		if status != http.StatusOK {
			t.Fatalf("reading %s = %d: %s", suffix, status, body)
		}
		var fields map[string]json.RawMessage
		decodeInto(t, body, &fields)
		field := "next"
		if suffix == "" {
			field = "turnsNext"
		}
		if string(fields[field]) != "null" {
			t.Errorf("exhausted %s must be present and null: %s", field, body)
		}
	}
}

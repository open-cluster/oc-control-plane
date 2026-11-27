package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestChatExecutionSnapshotUsesCommittedMessage(t *testing.T) {
	plane, _ := agentPlane(t, concludingModel{})
	base := plane.base(surfaceOrg) + "/conversations"
	status, body := plane.call(t, http.MethodPost, base, map[string]string{"title": "Snapshot"})
	var chat struct{ ID string }
	decodeInto(t, body, &chat)
	if status != http.StatusCreated {
		t.Fatalf("create Chat = %d: %s", status, body)
	}
	messages := "/conversations/" + chat.ID + "/messages"
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+messages,
		map[string]string{"clientMessageId": uuid.NewString(), "text": "What happened?"})
	var accepted struct{ ExecutionID, EventsURL string }
	decodeInto(t, body, &accepted)
	path := base + "/" + chat.ID + "/executions/" + accepted.ExecutionID
	if status != http.StatusAccepted || accepted.EventsURL != strings.TrimPrefix(path, "http://"+plane.api)+"/events" {
		t.Fatalf("admission did not expose the execution stream: %d %s", status, body)
	}
	awaitChatMessages(t, plane, messages, 2)
	status, body = plane.call(t, http.MethodGet, path, nil)
	var snapshot struct {
		Status, DraftText string
		Message           struct {
			Sequence int64
			Text     string
		}
	}
	decodeInto(t, body, &snapshot)
	if status != http.StatusOK || snapshot.Status != "completed" || snapshot.DraftText != "" ||
		snapshot.Message.Sequence != 2 || snapshot.Message.Text != "No cause established." {
		t.Fatalf("snapshot did not use the committed Message: %d %s", status, body)
	}
	status, body = plane.call(t, http.MethodPost, path+"/cancel", nil)
	if status != http.StatusOK || !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("cancellation replaced a committed reply: %d %s", status, body)
	}
	mismatched := base + "/" + uuid.NewString() + "/executions/" + accepted.ExecutionID
	if code, _ := plane.call(t, http.MethodGet, mismatched, nil); code != http.StatusNotFound {
		t.Fatalf("execution visible under an unrelated Conversation: %d", code)
	}
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+messages, nil)
	var history struct {
		Items []struct{ ExecutionID string }
	}
	decodeInto(t, body, &history)
	if status != http.StatusOK || len(history.Items) != 2 || history.Items[0].ExecutionID != accepted.ExecutionID || history.Items[1].ExecutionID != "" {
		t.Fatalf("history did not link the user Message to its execution: %d %s", status, body)
	}
}

func TestExplicitInvestigationDoesNotOwnChatContext(t *testing.T) {
	plane, _ := agentPlane(t, concludingModel{})
	base := plane.base(surfaceOrg)
	status, body := plane.call(t, http.MethodPost, base+"/conversations", map[string]string{"title": "Checkout"})
	if status != http.StatusCreated {
		t.Fatalf("create Chat = %d: %s", status, body)
	}
	var chat struct{ ID string }
	decodeInto(t, body, &chat)
	headers := http.Header{"Idempotency-Key": {uuid.NewString()}}
	request := map[string]string{"conversationId": chat.ID, "question": "Find the cause of checkout latency"}
	var started struct{ ID, ExecutionID string }
	status, body = plane.call(t, http.MethodPost, base+"/investigations", request, headers)
	if status != http.StatusAccepted {
		t.Fatalf("start linked Investigation = %d: %s", status, body)
	}
	decodeInto(t, body, &started)
	status, body = plane.call(t, http.MethodPost, base+"/investigations", request, headers)
	var replay struct{ ID, ExecutionID string }
	decodeInto(t, body, &replay)
	if status != http.StatusAccepted || started.ID == "" || started.ExecutionID == "" || replay != started {
		t.Fatalf("linked Investigation retry = %d: %s", status, body)
	}
	request["question"] = "Different task"
	if status, body = plane.call(t, http.MethodPost, base+"/investigations", request, headers); status != http.StatusConflict {
		t.Fatalf("conflicting request = %d: %s", status, body)
	}
	if body := plane.awaitInvestigation(t, started.ID); !strings.Contains(body, `"status":"concluded"`) {
		t.Fatalf("linked Investigation did not conclude: %s", body)
	}
	conn, err := pgx.Connect(t.Context(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var session string
	err = conn.QueryRow(t.Context(), `SELECT session_id FROM agent_checkpoint
		WHERE org_id = $1 AND writer_execution_id = $2`, surfaceOrg, started.ExecutionID).Scan(&session)
	if err != nil || session != "execution/"+started.ExecutionID {
		t.Fatalf("linked Investigation checkpoint = %q: %v", session, err)
	}
	status, body = plane.call(t, http.MethodGet, base+"/conversations/"+chat.ID+"/messages", nil)
	if status != http.StatusOK || !strings.Contains(body, `"items":[]`) {
		t.Fatalf("Investigating added a Chat Message = %d: %s", status, body)
	}
	status, body = plane.call(t, http.MethodDelete, base+"/conversations/"+chat.ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete Chat = %d: %s", status, body)
	}
	for _, suffix := range []string{"", "/messages", "/turns"} {
		if status, body := plane.call(t, http.MethodGet, base+"/conversations/"+chat.ID+suffix, nil); status != http.StatusNotFound {
			t.Fatalf("read deleted Chat at %q = %d: %s", suffix, status, body)
		}
	}
	if body := plane.awaitInvestigation(t, started.ID); !strings.Contains(body, `"status":"concluded"`) {
		t.Fatalf("Chat deletion changed a linked Investigation: %s", body)
	}
}

func TestWebChatRestoresContextAfterRestart(t *testing.T) {
	first, _ := agentPlane(t, concludingModel{})
	base := first.base(surfaceOrg) + "/conversations"
	status, body := first.call(t, http.MethodPost, base, map[string]string{"title": "Checkout"})
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, body)
	}
	var opened struct{ ID string }
	decodeInto(t, body, &opened)
	path := "/conversations/" + opened.ID + "/messages"
	submit := func(plane *integrationPlane, text string) {
		t.Helper()
		status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+path,
			map[string]string{"clientMessageId": uuid.NewString(), "text": text})
		if status != http.StatusAccepted {
			t.Fatalf("submit %q = %d: %s", text, status, body)
		}
	}
	submit(first, "Remember the amber deployment")
	awaitChatMessages(t, first, path, 2)
	first.shutdown()

	prompts := make(chan []*message.Msg, 2)
	address := freeAddress(t)
	restarted := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = first.dsn
		cfg.HTTPListenAddress = address
	}, app.Options{Model: concludingModel{prompts: prompts}})
	restarted.sessionCookie = first.sessionCookie
	second := &integrationPlane{controlPlane: restarted, api: address, intake: address}
	submit(second, "Which deployment did I mention?")
	submit(second, "What was the previous question?")

	for i, wanted := range []string{"Which deployment did I mention?", "What was the previous question?"} {
		select {
		case prompt := <-prompts:
			encoded, err := json.Marshal(prompt)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), "Remember the amber deployment") ||
				!strings.Contains(string(encoded), "No cause established.") ||
				!strings.Contains(string(encoded), wanted) {
				t.Fatalf("reply %d lost context or ran out of order: %s", i, encoded)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no model call for %q", wanted)
		}
	}
	awaitChatMessages(t, second, path, 6)
	status, body = second.call(t, http.MethodGet, second.base(surfaceOrg)+path+"?limit=2", nil)
	var page struct {
		Items []struct{ Sequence int64 }
		Next  string
	}
	decodeInto(t, body, &page)
	if status != http.StatusOK || page.Next == "" || len(page.Items) != 2 ||
		page.Items[0].Sequence != 1 || page.Items[1].Sequence != 2 {
		t.Fatalf("first page = %d: %s", status, body)
	}
	status, body = second.call(t, http.MethodGet, second.base(surfaceOrg)+path+"?limit=2&cursor="+page.Next, nil)
	decodeInto(t, body, &page)
	if status != http.StatusOK || len(page.Items) != 2 || page.Items[0].Sequence != 3 || page.Items[1].Sequence != 4 {
		t.Fatalf("second page = %d: %s", status, body)
	}
}

func awaitChatMessages(t *testing.T, plane *integrationPlane, path string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		var status int
		status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
		if status != http.StatusOK {
			t.Fatalf("read Chat = %d: %s", status, body)
		}
		var history struct {
			Messages []struct{ Role string } `json:"items"`
		}
		decodeInto(t, body, &history)
		if len(history.Messages) == count && history.Messages[count-1].Role == "assistant" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("waiting for %d visible Chat Messages: %s", count, body)
}

type titleModel struct {
	concludingModel
	started chan []*message.Msg
	release chan struct{}
	fails   bool
}

func (m titleModel) Chat(ctx context.Context, prompt []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if len(prompt) == 1 {
		var options model.CallOptions
		for _, option := range opts {
			option(&options)
		}
		if len(options.Tools) != 0 {
			return nil, errors.New("title request offered Tools")
		}
		select {
		case m.started <- prompt:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if m.fails {
			return nil, errors.New("title provider failed")
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		reply := investigationRecap()
		reply.Content = []message.ContentBlock{message.TextBlock{Type: "text", Text: "Checkout Releases"}}
		return reply, nil
	}
	return m.concludingModel.Chat(ctx, prompt, opts...)
}

func TestChatKeepsFallbackWhenTitleModelFails(t *testing.T) {
	release := make(chan struct{})
	close(release)
	model := titleModel{started: make(chan []*message.Msg, 1), release: release, fails: true}
	plane, _ := agentPlane(t, model)
	id, _ := plane.openConversation(t, "", "")
	path := "/conversations/" + id
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+path+"/messages",
		map[string]string{"clientMessageId": uuid.NewString(), "text": "Checkout latency"})
	if status != http.StatusAccepted {
		t.Fatalf("send = %d: %s", status, body)
	}
	awaitChatMessages(t, plane, path+"/messages", 2)
	select {
	case <-model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("first successful exchange did not try to name the Conversation")
	}
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
	if status != http.StatusOK || !strings.Contains(body, `"title":"Checkout latency"`) {
		t.Fatalf("read after title model failure = %d: %s", status, body)
	}
}

func TestChatTitleDoesNotBlockRepliesOrReplaceRename(t *testing.T) {
	model := titleModel{started: make(chan []*message.Msg, 2), release: make(chan struct{})}
	plane, _ := agentPlane(t, model)
	base := plane.base(surfaceOrg) + "/conversations"
	status, body := plane.call(t, http.MethodPost, base, map[string]string{})
	if status != http.StatusCreated {
		t.Fatalf("opening Chat = %d: %s", status, body)
	}
	var opened struct{ ID string }
	decodeInto(t, body, &opened)
	path := "/conversations/" + opened.ID
	send := func(text string) {
		t.Helper()
		status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+path+"/messages",
			map[string]string{"clientMessageId": uuid.NewString(), "text": text})
		if status != http.StatusAccepted {
			t.Fatalf("submitting Message = %d: %s", status, body)
		}
	}
	send("What changed?\n\tIn checkout?")
	select {
	case <-model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first successful exchange did not request a title")
	}
	awaitChatMessages(t, plane, path+"/messages", 2)
	status, body = plane.call(t, http.MethodGet, plane.base(surfaceOrg)+path, nil)
	if status != http.StatusOK || !strings.Contains(body, `"title":"What changed? In checkout?"`) {
		t.Fatalf("fallback title while provider runs = %d: %s", status, body)
	}
	send("Any other changes?")
	awaitChatMessages(t, plane, path+"/messages", 4)
	status, body = plane.call(t, http.MethodPatch, plane.base(surfaceOrg)+path, map[string]string{"title": "My title"})
	if status != http.StatusOK || !strings.Contains(body, `"title":"My title"`) {
		t.Fatalf("rename = %d: %s", status, body)
	}
	close(model.release)
	plane.shutdown()

	conn, err := pgx.Connect(t.Context(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var title string
	err = conn.QueryRow(t.Context(), `SELECT title FROM conversation
		WHERE org_id = $1 AND conversation_id = $2`, surfaceOrg, opened.ID).Scan(&title)
	if err != nil || title != "My title" {
		t.Fatalf("stored title after generation = %q: %v", title, err)
	}
	if len(model.started) != 0 {
		t.Fatal("a follow-up requested a title")
	}
}

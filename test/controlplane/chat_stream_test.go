package controlplane

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

func TestChatTextStreamsThroughProxyBeforeCommit(t *testing.T) {
	const secret = "streaming-canary-provider-key"
	const private = "private-provider-reasoning"
	const chunks = 120
	start := make(chan struct{})
	tail := make(chan struct{})
	first := make(chan time.Time, 1)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("reconnecting repeated the provider call")
			http.Error(w, "unexpected model call", http.StatusBadRequest)
			return
		}
		select {
		case <-start:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk := func(content, reasoning, finish string) {
			_, _ = fmt.Fprintf(w, `data: {"id":"reply","model":"fixture","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":%q,"reasoning":%q},"finish_reason":%s}]}`+"\n\n",
				content, reasoning, finish)
			_ = http.NewResponseController(w).Flush()
		}
		first <- time.Now()
		writeChunk("Visible preview "+secret[:16], private, "null")
		select {
		case <-tail:
		case <-r.Context().Done():
			return
		}
		writeChunk(secret[16:]+" ", "", "null")
		for range chunks {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				writeChunk("x", "", "null")
			}
		}
		writeChunk(".", "", `"stop"`)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)

	address := freeAddress(t)
	var dsn string
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "fixture", secret
		dsn = cfg.DatabaseDSN
	}, app.Options{ModelBaseURL: provider.URL})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	readAddress := freeAddress(t)
	replica := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = readAddress
		cfg.DatabaseDSN = dsn
	}, app.Options{})
	reconnect := &integrationPlane{controlPlane: replica, api: readAddress}
	reconnect.sessionCookie = plane.sessionCookie
	proxied := *plane
	proxied.api = startEventProxy(t, address)
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations", map[string]string{"title": "Streaming"})
	var chat struct{ ID string }
	decodeInto(t, body, &chat)
	if status != http.StatusCreated {
		t.Fatalf("create Chat = %d: %s", status, body)
	}
	proxied.sessionCookie = plane.sessionCookie
	status, body = plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/conversations/"+chat.ID+"/messages",
		map[string]string{"clientMessageId": uuid.NewString(), "text": "Show the state of checkout"})
	var accepted struct{ EventsURL string }
	decodeInto(t, body, &accepted)
	if status != http.StatusAccepted || accepted.EventsURL == "" {
		t.Fatalf("accept Chat = %d: %s", status, body)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	if _, err := conn.Exec(t.Context(), "CREATE EXTENSION pg_stat_statements"); err != nil {
		t.Fatal(err)
	}

	response := openSnapshotStream(t, &proxied, accepted.EventsURL)
	defer func() { _ = response.Body.Close() }()
	close(start)
	scanner := bufio.NewScanner(response.Body)
	readSnapshot := func(scanner *bufio.Scanner) (conversation.ExecutionSnapshot, bool) {
		t.Helper()
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			if strings.Contains(data, secret[:16]) || strings.Contains(data, private) {
				t.Fatal("a snapshot exposed the model key or private reasoning")
			}
			var snapshot conversation.ExecutionSnapshot
			if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
				t.Fatal(err)
			}
			return snapshot, true
		}
		return conversation.ExecutionSnapshot{}, false
	}
	for {
		snapshot, open := readSnapshot(scanner)
		if !open || snapshot.Terminal() {
			t.Fatalf("stream ended before provisional text: %v", scanner.Err())
		}
		if snapshot.DraftText == "Visible preview " {
			break
		}
	}
	latency := time.Since(<-first)
	if latency > 1500*time.Millisecond {
		t.Errorf("first provisional text through the proxy took %s", latency)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	reconnected := openSnapshotStream(t, reconnect, accepted.EventsURL)
	defer func() { _ = reconnected.Body.Close() }()
	scanner = bufio.NewScanner(reconnected.Body)
	if snapshot, open := readSnapshot(scanner); !open || snapshot.DraftText != "Visible preview " || snapshot.Message != nil {
		t.Fatal("reconnect lost the current draft or published the Message before commit")
	}
	close(tail)
	var final conversation.ExecutionSnapshot
	for snapshot, open := readSnapshot(scanner); open; snapshot, open = readSnapshot(scanner) {
		final = snapshot
	}
	want := "Visible preview [redacted] " + strings.Repeat("x", chunks) + "."
	if final.Status != "completed" || final.DraftText != "" || final.Message == nil ||
		final.Message.Sequence != 2 || final.Message.Text != want || scanner.Err() != nil {
		t.Fatalf("invalid terminal Chat snapshot: status=%s, committed=%v, error=%v", final.Status, final.Message != nil, scanner.Err())
	}
	var writes, reads int
	err = conn.QueryRow(t.Context(), `
		SELECT coalesce(sum(calls) FILTER (WHERE query LIKE 'UPDATE agent_execution SET draft_text = nullif%'), 0),
		       coalesce(sum(calls) FILTER (WHERE query LIKE 'SELECT e.execution_id, c.conversation_id,%activity.tools%'), 0)
		  FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&writes, &reads)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first text through proxy in %s, %d draft writes and %d snapshot reads for %d trailing deltas",
		latency, writes, reads, chunks)
	if writes < 2 || writes > 12 || reads < 3 || reads > 12 {
		t.Fatalf("expected bounded database activity, got %d writes and %d reads", writes, reads)
	}
}

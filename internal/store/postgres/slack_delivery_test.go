package storage_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func slackDelivery(t *testing.T, handler http.Handler) (*storage.Database, tenancy.Organization, uuid.UUID, slack.Worker) {
	t.Helper()
	database, org := migratedDatabase(t)
	id, integration := aSlackTurn(t, database, org, "T1", "C1", "1700000000.1")
	material := make([]byte, seal.KeyLength)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(material)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("test-slack-credential", integrations.CredentialBinding(integration))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Pool(org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE integration SET credential_sealed = $3,
		credential_fingerprint = 'fixture', credential_created_at = now(), credential_key_id = 'default'
		WHERE org_id = $1 AND integration_id = $2`, org.String(), integration, sealed); err != nil {
		t.Fatal(err)
	}
	vendor := httptest.NewServer(handler)
	t.Cleanup(vendor.Close)
	return database, org, id, slack.Worker{Replies: database, Sealer: sealer, Client: slack.NewClient(vendor.URL),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: 20 * time.Millisecond, Batch: 1}
}

func TestSlackRetriesTerminalCloseAfterCursorAdvanced(t *testing.T) {
	t.Parallel()
	var appends, stops atomic.Int32
	database, org, id, worker := slackDelivery(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.appendStream" {
			appends.Add(1)
		}
		if r.URL.Path == "/chat.stopStream" && stops.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"ok":false,"error":"temporary_failure"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1700000100.1"}`)
	}))
	ctx := context.Background()
	if err := database.ConcludeInvestigation(ctx, org, id, claimToken(t, database, org, id), conclusionSaying("durable answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	runSlackWorker(t, worker)
	deadline := time.Now().Add(8 * time.Second)
	for {
		status, _, _, _, _, err := database.SlackReplyState(ctx, org, id)
		if err != nil {
			t.Fatal(err)
		}
		if status == storage.SlackReplyDelivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal close did not recover: appends=%d stops=%d", appends.Load(), stops.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if appends.Load() != 1 || stops.Load() != 2 {
		t.Fatalf("acknowledged content repeated: appends=%d stops=%d", appends.Load(), stops.Load())
	}
}

func runSlackWorker(t *testing.T, worker slack.Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func awaitSlackText(t *testing.T, sent <-chan string, expected string) {
	t.Helper()
	select {
	case text := <-sent:
		if !strings.Contains(text, expected) {
			t.Fatalf("Slack text %q lacks %q", text, expected)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("Slack did not receive %q at the flush cadence", expected)
	}
}

func TestSlackNonterminalPassesReleaseRecoveryLease(t *testing.T) {
	for _, initial := range []string{"empty", "held", "progress"} {
		t.Run(initial, func(t *testing.T) {
			t.Parallel()
			sent := make(chan string, 8)
			database, org, id, worker := slackDelivery(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.URL.Path == "/chat.appendStream" {
					sent <- r.FormValue("markdown_text")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true,"ts":"1700000100.1"}`)
			}))
			ctx := context.Background()
			token := claimToken(t, database, org, id)
			if initial != "empty" {
				event := investigation.Event{Type: investigation.EventStarted, Payload: map[string]any{}}
				if initial == "progress" {
					event = investigation.Event{Type: investigation.EventToolCompleted, Payload: map[string]any{"summary": "first batch"}}
				}
				if err := database.AppendEvent(ctx, org, id, token, event); err != nil {
					t.Fatal(err)
				}
			}
			runSlackWorker(t, worker)
			if initial == "progress" {
				awaitSlackText(t, sent, "first batch")
			} else {
				pool, err := database.Pool(org)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(4 * time.Second)
				for {
					var released bool
					if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_reply
						WHERE org_id = $1 AND investigation_id = $2 AND status = 1
						AND lease_owner IS NULL AND leased_until IS NULL AND next_attempt_at > now())`, org.String(), id).Scan(&released); err != nil {
						t.Fatal(err)
					}
					if released {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("worker did not release the empty or held pass for its next flush")
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			if err := database.AppendEvent(ctx, org, id, token, investigation.Event{Type: investigation.EventToolCompleted, Payload: map[string]any{"summary": "next batch"}}); err != nil {
				t.Fatal(err)
			}
			awaitSlackText(t, sent, "next batch")
		})
	}
}

package storage_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackAcceptanceBeforeIdentityPersistenceIsAtLeastOnce(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			t.Parallel()
			var messages atomic.Int32
			database, org, id, worker := slackDelivery(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/chat.startStream" && !native {
					_, _ = io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
					return
				}
				if r.URL.Path == "/chat.startStream" || r.URL.Path == "/chat.postMessage" {
					_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1700000100.%d"}`, messages.Add(1))
					return
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			ctx := context.Background()
			if err := database.ConcludeInvestigation(ctx, org, id, claimToken(t, database, org, id), conclusionSaying("durable answer"), "", investigation.Usage{}); err != nil {
				t.Fatal(err)
			}
			pool, err := database.Pool(org)
			if err != nil {
				t.Fatal(err)
			}
			barrier, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Release()
			if _, err = barrier.Exec(ctx, `SELECT pg_advisory_lock(4868)`); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = barrier.Exec(ctx, `SELECT pg_advisory_unlock(4868)`) }()
			if _, err = pool.Exec(ctx, `CREATE FUNCTION pause_slack_identity() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF OLD.stream_ts = '' AND NEW.stream_ts <> '' THEN PERFORM pg_advisory_xact_lock(4868); END IF; RETURN NEW; END $$;
				CREATE TRIGGER pause_slack_identity BEFORE UPDATE ON slack_reply FOR EACH ROW EXECUTE FUNCTION pause_slack_identity()`); err != nil {
				t.Fatal(err)
			}
			stop := runSlackWorker(t, worker)
			deadline := time.Now().Add(5 * time.Second)
			for {
				var blocked bool
				if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event = 'advisory')`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("identity persistence never reached the acceptance barrier")
				}
				time.Sleep(20 * time.Millisecond)
			}
			stop()
			if messages.Load() != 1 {
				t.Fatalf("provider accepted %d messages before interruption", messages.Load())
			}
			if _, err = barrier.Exec(ctx, `SELECT pg_advisory_unlock(4868)`); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `DROP TRIGGER pause_slack_identity ON slack_reply; DROP FUNCTION pause_slack_identity()`); err != nil {
				t.Fatal(err)
			}
			_, sequence, message, _, found, err := database.SlackReplyState(ctx, org, id)
			if err != nil || !found || sequence != 0 || message != "" {
				t.Fatalf("interrupted identity was persisted: %d %q %v %v", sequence, message, found, err)
			}
			if _, err = pool.Exec(ctx, `UPDATE slack_reply SET leased_until = now() - interval '1 second' WHERE org_id = $1 AND investigation_id = $2`, org.String(), id); err != nil {
				t.Fatal(err)
			}
			runSlackWorker(t, worker)
			deadline = time.Now().Add(5 * time.Second)
			for {
				status, _, message, _, _, readErr := database.SlackReplyState(ctx, org, id)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if status == storage.SlackReplyDelivered {
					if message != "1700000100.2" || messages.Load() != 2 {
						t.Fatalf("retry identity=%q accepted messages=%d", message, messages.Load())
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("replacement worker did not finish delivery")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

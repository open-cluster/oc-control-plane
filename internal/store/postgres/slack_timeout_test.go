package storage_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestSlackAttemptTimeoutPreservesRetryBudget(t *testing.T) {
	for _, previous := range []int{0, 7} {
		t.Run(fmt.Sprintf("previous=%d", previous), func(t *testing.T) {
			t.Parallel()
			database, org, id, worker := slackDelivery(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}))
			ctx := context.Background()
			if err := database.ConcludeInvestigation(ctx, org, id, claimToken(t, database, org, id), conclusionSaying("durable answer"), "", investigation.Usage{}); err != nil {
				t.Fatal(err)
			}
			pool, err := database.Pool(org)
			if err != nil {
				t.Fatal(err)
			}
			// Shorten the actual database claim, preserving the worker's deadline calculation.
			if _, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION short_slack_claim() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF NEW.lease_owner IS NOT NULL AND NEW.lease_owner IS DISTINCT FROM OLD.lease_owner THEN
				NEW.leased_until := clock_timestamp() + interval '2 seconds'; NEW.attempts := %d;
				END IF; RETURN NEW; END $$;
				CREATE TRIGGER short_slack_claim BEFORE UPDATE ON slack_reply FOR EACH ROW EXECUTE FUNCTION short_slack_claim()`, previous)); err != nil {
				t.Fatal(err)
			}
			stop := runSlackWorker(t, worker)
			deadline := time.Now().Add(5 * time.Second)
			for {
				var attempts, status int
				var settled bool
				if err = pool.QueryRow(ctx, `SELECT attempts, status, lease_owner IS NULL AND leased_until IS NULL AND next_attempt_at > now()
					FROM slack_reply WHERE org_id = $1 AND investigation_id = $2`, org.String(), id).Scan(&attempts, &status, &settled); err == nil && attempts == previous+1 {
					want := storage.SlackReplyPending
					if previous == 7 {
						want = storage.SlackReplyFailed
					}
					if status != want || !settled {
						t.Fatalf("timeout settlement: status=%d attempts=%d released/backoff=%v", status, attempts, settled)
					}
					stop()
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("timeout bypassed retry accounting: attempts=%d status=%d err=%v", attempts, status, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

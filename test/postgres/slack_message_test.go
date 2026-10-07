package storage_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
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
			pool, err := poolForTest(database, org)
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
			_, sequence, message, _, found, err := slackReplyStateForTest(database, ctx, org, id)
			if err != nil || !found || sequence != 0 || message != "" {
				t.Fatalf("interrupted identity was persisted: %d %q %v %v", sequence, message, found, err)
			}
			if _, err = pool.Exec(ctx, `UPDATE slack_reply SET leased_until = now() - interval '1 second' WHERE org_id = $1 AND investigation_id = $2`, org, id); err != nil {
				t.Fatal(err)
			}
			runSlackWorker(t, worker)
			deadline = time.Now().Add(5 * time.Second)
			for {
				status, _, message, _, _, readErr := slackReplyStateForTest(database, ctx, org, id)
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

func slackDelivery(t *testing.T, handler http.Handler) (*storage.Database, uuid.UUID, uuid.UUID, slack.Worker) {
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
	pool, err := poolForTest(database, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE integration SET credential_sealed = $3
		WHERE org_id = $1 AND integration_id = $2`, org, integration, sealed); err != nil {
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
		status, _, _, _, _, err := slackReplyStateForTest(database, ctx, org, id)
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
				pool, err := poolForTest(database, org)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(4 * time.Second)
				for {
					var released bool
					if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_reply
						WHERE org_id = $1 AND investigation_id = $2 AND status = 1
						AND lease_owner IS NULL AND leased_until IS NULL AND next_attempt_at > now())`, org, id).Scan(&released); err != nil {
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

func TestSlackMessageCapacityDeferralDoesNotConsumeRetries(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	chat := openConversation(t, database, organization, "pending work")
	say(t, database, organization, chat.ID, "hold the pending slot")
	if _, opened, err := openTurnForTest(database, ctx, organization, chat.ID, turnWindowLead); err != nil || !opened {
		t.Fatalf("occupying capacity: %v, %v", opened, err)
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	worker := fixture.worker
	worker.MaxWaitingTurns = 1
	worker.MaxAttempts = 1
	for range 3 {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatalf("deferring accepted message: %v, %v", worked, err)
		}
		work := readSlackMessageWork(t, fixture)
		if work.Status != storage.SlackMessageRetry || work.Attempts != 0 {
			t.Fatalf("capacity deferral consumed retry budget: %+v", work)
		}
		if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET available_at = now() - interval '1 second'
			WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("investigation-worker")); err != nil || !claimed {
		t.Fatalf("releasing the pending slot: %v, %v", claimed, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing after capacity becomes available: %v, %v", worked, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageComplete || work.Attempts != 1 {
		t.Fatalf("deferred message did not recover: %+v", work)
	}
}

func TestSlackMessageLeaseRejectsOtherOrganizationsAndSupersededOwners(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	first, found, err := database.ClaimSlackMessageWork(ctx, "first-owner", time.Minute)
	if err != nil || !found {
		t.Fatalf("initial claim: %+v, %v, %v", first, found, err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, fixture.other, first, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization renewed the lease: %v", err)
	}
	if err := database.DeferSlackMessageWork(ctx, fixture.other, first, time.Second); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization deferred the message: %v", err)
	}
	if err := database.FailSlackMessageWork(ctx, fixture.other, first, true, 0, "foreign", "foreign"); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("another Organization failed the message: %v", err)
	}
	if err := database.ApplySlackMessageWork(ctx, fixture.other, first, time.Hour, 0); err == nil {
		t.Fatal("another Organization applied the Message")
	}
	forged := first
	forged.LeaseEpoch++
	if err := database.HeartbeatSlackMessageWork(ctx, organization, forged, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("incorrect epoch renewed the lease: %v", err)
	}
	forged = first
	forged.LeaseOwner = "incorrect-owner"
	if err := database.HeartbeatSlackMessageWork(ctx, organization, forged, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("incorrect owner renewed the lease: %v", err)
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatSlackMessageWork(ctx, organization, first, time.Minute); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
		t.Fatalf("expired lease was resurrected: %v", err)
	}
	replacement, found, err := database.ClaimSlackMessageWork(ctx, "replacement-owner", time.Minute)
	if err != nil || !found || replacement.ID != first.ID || replacement.LeaseEpoch != first.LeaseEpoch+1 {
		t.Fatalf("replacement claim did not advance fencing: %+v, %v, %v", replacement, found, err)
	}
	for name, attempt := range map[string]func() error{
		"heartbeat": func() error { return database.HeartbeatSlackMessageWork(ctx, organization, first, time.Minute) },
		"deferral":  func() error { return database.DeferSlackMessageWork(ctx, organization, first, time.Second) },
		"failure": func() error {
			return database.FailSlackMessageWork(ctx, organization, first, true, 0, "stale", "stale")
		},
		"source reference": func() error {
			return database.SetSlackMessageSourceReference(ctx, organization, fixture.conversation,
				first.MessageSequence, "https://test.slack.com/archives/stale", first)
		},
		"turn creation": func() error { return database.ApplySlackMessageWork(ctx, organization, first, time.Hour, 0) },
	} {
		if err := attempt(); !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
			t.Fatalf("superseded %s was accepted: %v", name, err)
		}
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 || detail.Messages[0].SourceReference != "" {
		t.Fatalf("stale owner left partial effects: %+v, %v", detail, err)
	}
	if err := database.ApplySlackMessageWork(ctx, organization, replacement, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 {
		t.Fatalf("replacement could not open the turn: %+v, %v", detail, err)
	}
}

func TestSlackMessageHeartbeatProtectsSlowProviderLookup(t *testing.T) {
	database, organization := migratedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	integration, err := connectSlack(t, database, organization, "Slack", slackInstallation("THEARTBEAT"))
	if err != nil {
		t.Fatal(err)
	}
	material := make([]byte, seal.KeyLength)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(material)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("test-slack-message-credential", integrations.CredentialBinding(integration.ID))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integration SET credential_sealed = $3
		WHERE org_id = $1 AND integration_id = $2`, organization, integration.ID, sealed); err != nil {
		t.Fatal(err)
	}
	outcome, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: integration.ID, ContentDigest: randomDigest(t),
		Channel: "CHEARTBEAT", Thread: "1700000001.000001", MessageID: "1700000001.000001",
		Subject: "slow provider", ActorID: "UWORK", Text: "why?",
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"url":"https://test.slack.com/","user_id":"UWORK","team_id":"THEARTBEAT"}`)
	}))
	t.Cleanup(vendor.Close)
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	worker := slackwork.MessageWorker{
		Database: database, Owner: "slow-lookup-worker", Lease: 600 * time.Millisecond, WindowLead: time.Hour,
		References: &slackwork.SlackReferenceResolver{
			Database: database, Client: slack.NewClient(vendor.URL), Sealer: sealer,
		},
	}
	finished := make(chan error, 1)
	go func() {
		worked, err := worker.ProcessOne(ctx)
		if err == nil && !worked {
			err = errors.New("accepted message was not claimed")
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("accepted message never reached the provider")
	}
	select {
	case <-time.After(900 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if work, found, err := database.ClaimSlackMessageWork(ctx, "replacement-worker", time.Minute); err != nil || found {
		t.Fatalf("slow lookup lost its renewed lease: %+v, %v, %v", work, found, err)
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	detail, err := database.ConversationDetail(ctx, organization, outcome.Conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 1 ||
		detail.Messages[0].SourceReference != "https://test.slack.com/archives/CHEARTBEAT/p1700000001000001" {
		t.Fatalf("slow lookup lost the accepted turn or source reference: %+v, %v", detail, err)
	}
}

func TestSlackMessageRetryTerminatesAndReplayPreservesTheQuestion(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_slack_turn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected turn failure'; END $$;
		CREATE TRIGGER reject_slack_turn BEFORE INSERT ON investigation
		FOR EACH ROW EXECUTE FUNCTION reject_slack_turn()`); err != nil {
		t.Fatal(err)
	}
	worker := fixture.worker
	worker.MaxAttempts = 3
	worker.RetryBase = time.Minute
	for attempt := 1; attempt <= 3; attempt++ {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatalf("attempt %d: %v, %v", attempt, worked, err)
		}
		work := readSlackMessageWork(t, fixture)
		if work.Attempts != attempt {
			t.Fatalf("attempt %d work: %+v", attempt, work)
		}
		if attempt < 3 {
			if work.Status != storage.SlackMessageRetry {
				t.Fatalf("retry was not scheduled: %+v", work)
			}
			if delay := work.AvailableAt.Sub(work.UpdatedAt); delay != time.Duration(attempt)*time.Minute {
				t.Fatalf("attempt %d retry delay = %s, want %d minutes", attempt, delay, attempt)
			}
			if worked, err := worker.ProcessOne(ctx); err != nil || worked {
				t.Fatalf("retry ignored backoff: %v, %v", worked, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE slack_message_work SET available_at = now() - interval '1 second'
				WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
				t.Fatal(err)
			}
		} else if work.Status != storage.SlackMessageTerminal || work.FailureClass != "provider-job-failed" {
			t.Fatalf("exhausted retry was not terminal: %+v", work)
		}
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("terminal work was reclaimed: %v, %v", worked, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Messages) != 1 || len(detail.Turns) != 0 {
		t.Fatalf("failed turn lost or partially processed the question: %+v, %v", detail, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_slack_turn ON investigation; DROP FUNCTION reject_slack_turn()`); err != nil {
		t.Fatal(err)
	}
	if err := database.RecoverSlackMessage(ctx, ownerOf(t, organization), organization,
		fixture.conversation, 1); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing replay: %v, %v", worked, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Messages) != 1 || len(detail.Turns) != 1 {
		t.Fatalf("replay changed the question or duplicated a turn: %+v, %v", detail, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageComplete || work.Attempts != 1 {
		t.Fatalf("recovered work: %+v", work)
	}
}

func TestSlackMessageRecoveryAtPersistedAttemptLimitIsTerminal(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	ctx := context.Background()
	database, organization := fixture.database, fixture.organization
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE slack_message_work
		SET status = 2, attempts = 12, lease_owner = 'interrupted-worker', lease_epoch = 12,
		    lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND delivery_id = $2`, organization, fixture.delivery); err != nil {
		t.Fatal(err)
	}
	if worked, err := fixture.worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("recovering exhausted work: %v, %v", worked, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageTerminal || work.Attempts != 12 {
		t.Fatalf("recovery exceeded the persisted budget or failed to stop: %+v", work)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("exhausted work opened a turn or lost the question: %+v, %v", detail, err)
	}
}

func TestSlackMessageWorkerOpensAcceptedTurnExactlyOnce(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	database, organization := fixture.database, fixture.organization
	ctx := context.Background()
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("acknowledged message before processing: %+v, %v", detail, err)
	}
	worker := fixture.worker
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("processing accepted message: %v, %v", worked, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("processing completed message again: %v, %v", worked, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 1 {
		t.Fatalf("processed message: %+v, %v", detail, err)
	}
	work := readSlackMessageWork(t, fixture)
	if work.Status != storage.SlackMessageComplete || work.Attempts != 1 {
		t.Fatalf("processed work: %+v", work)
	}
	if _, err := database.ConversationDetail(ctx, fixture.other, fixture.conversation, 50); err == nil {
		t.Fatal("another Organization could read the processed Conversation")
	}
}

func TestSlackMessageWorkerQueuesFollowupUntilTheCurrentTurnEnds(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	database, organization, worker := fixture.database, fixture.organization, fixture.worker
	ctx := context.Background()
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("initial message: %v, %v", worked, err)
	}
	if _, err := database.RecordSlackMessage(ctx, organization, storage.SlackMessage{
		Integration: fixture.integration, ContentDigest: randomDigest(t), Channel: "CWORK", Thread: "1.0",
		Subject: "follow-up", ActorID: "UWORK", Text: "check the next thing",
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("follow-up message: %v, %v", worked, err)
	}
	detail, err := database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 1 || len(detail.Messages) != 2 || !detail.Messages[1].Queued() {
		t.Fatalf("follow-up did not wait for the current turn: %+v, %v", detail, err)
	}
	first := detail.Turns[0].InvestigationID
	if err := database.ConcludeInvestigation(ctx, organization, first, claimToken(t, database, organization, first),
		conclusionSaying("first answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if drained, err := database.DrainQueuedConversation(ctx, worker.WindowLead, 0); err != nil || !drained {
		t.Fatalf("draining the queued follow-up: %v, %v", drained, err)
	}
	detail, err = database.ConversationDetail(ctx, organization, fixture.conversation, 50)
	if err != nil || len(detail.Turns) != 2 || len(detail.Messages) != 2 ||
		detail.Messages[1].InvestigationID != detail.Turns[1].InvestigationID {
		t.Fatalf("queued follow-up lost its next turn: %+v, %v", detail, err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || worked {
		t.Fatalf("completed follow-up work was reclaimed: %v, %v", worked, err)
	}
}

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
			pool, err := poolForTest(database, org)
			if err != nil {
				t.Fatal(err)
			}
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
					FROM slack_reply WHERE org_id = $1 AND investigation_id = $2`, org, id).Scan(&attempts, &status, &settled); err == nil && attempts == previous+1 {
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

func TestFreshSlackWorkSchemaContainsOnlySlackState(t *testing.T) {
	database, organization := migratedDatabase(t)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, assertion := range []string{
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND
		 ((table_name = 'investigation' AND column_name = 'webhook_job_id') OR
		  (table_name = 'slack_message_work' AND column_name IN ('incident_id', 'kind'))))`,
		`SELECT count(*) = 2 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'slack_message_work'
		 AND column_name IN ('conversation_id', 'message_sequence') AND is_nullable = 'NO'`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN
		 ('investigation_webhook_job_is_in_the_same_org', 'slack_message_work_incident_is_in_the_same_org',
		  'slack_message_work_kind_check', 'slack_message_work_has_one_effect_reference'))`,
		`SELECT to_regclass('investigation_webhook_job_is_unique') IS NULL`,
		`SELECT to_regclass('webhook_job') IS NULL`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'slack_message_work' AND column_name = 'work_id')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname LIKE 'webhook_job%')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_class WHERE relname LIKE 'webhook_job%')`,
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) =
		 ARRAY['delivery_id','org_id','integration_id','content_digest','truncated',
		       'received_at','provider_identity','lifecycle_phase']
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'webhook_delivery'`,
	} {
		var valid bool
		if err := pool.QueryRow(context.Background(), assertion).Scan(&valid); err != nil || !valid {
			t.Fatalf("Slack-only schema contract failed for %s: %v", assertion, err)
		}
	}
}

func TestSlackWorkSchemaRejectsDuplicateAndCrossOrganizationReferences(t *testing.T) {
	fixture := acceptedSlackMessage(t)
	pool, err := poolForTest(fixture.database, fixture.organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name         string
		organization uuid.UUID
		sequence     any
		code         string
	}{
		{"duplicate Message effect", fixture.organization, int64(1), "23505"},
		{"another Organization", fixture.other, int64(1), "23503"},
		{"missing Message", fixture.organization, nil, "23502"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), `INSERT INTO slack_message_work
				(work_id, org_id, delivery_id, integration_id, conversation_id, message_sequence, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, now())`, uuid.New(), scenario.organization,
				fixture.delivery, fixture.integration, fixture.conversation, scenario.sequence)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != scenario.code {
				t.Fatalf("invalid Slack reference was not rejected with %s: %v", scenario.code, err)
			}
		})
	}
}

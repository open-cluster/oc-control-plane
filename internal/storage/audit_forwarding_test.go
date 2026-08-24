package storage_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

type recordingAuditForwarder struct {
	fail    bool
	events  []audit.Recorded
	failure error
}

func (f *recordingAuditForwarder) Forward(_ context.Context, event audit.Recorded) error {
	f.events = append(f.events, event)
	if f.fail {
		return f.failure
	}
	return nil
}

func TestAuditForwardingRetriesIdempotentlyThenRemovesTheOutboxRow(t *testing.T) {
	database, organization := migratedDatabase(t)
	database.EnableAuditForwarding()

	_, err := database.CreateIntegration(context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			Type:                     integrations.TypeAlertmanager,
			Name:                     "forwarded audit integration",
			WebhookSecretDigest:      randomDigest(t),
			WebhookSecretFingerprint: "minted-fingerprint",
		})
	if err != nil {
		t.Fatalf("creating audited integration: %v", err)
	}

	const secretInRemoteError = "remote-error-must-not-be-persisted-or-logged"
	forwarder := &recordingAuditForwarder{fail: true, failure: errors.New(secretInRemoteError)}
	var logs bytes.Buffer
	worker := audit.ForwardingWorker{
		Outbox:      database,
		Forwarder:   forwarder,
		Owner:       "worker-a",
		Lease:       time.Minute,
		RetryBase:   time.Second,
		MaxAttempts: 3,
		Batch:       10,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	}
	now := time.Now().UTC().Add(time.Second)
	if _, err = worker.DeliverReady(context.Background(), now); err != nil {
		t.Fatalf("first delivery sweep: %v", err)
	}
	if len(forwarder.events) != 1 {
		t.Fatalf("forward attempts = %d, want 1", len(forwarder.events))
	}
	eventID := forwarder.events[0].ID

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	var attempts int
	var terminal bool
	var lastError string
	if err = pool.QueryRow(context.Background(), `
		SELECT attempts, terminal, last_error
		  FROM audit_forwarding_outbox
		 WHERE event_id = $1`, eventID).Scan(&attempts, &terminal, &lastError); err != nil {
		t.Fatalf("reading retry state: %v", err)
	}
	if attempts != 1 || terminal || strings.Contains(lastError, secretInRemoteError) ||
		strings.Contains(logs.String(), secretInRemoteError) {
		t.Fatalf("unsafe retry state attempts=%d terminal=%t error=%q logs=%q",
			attempts, terminal, lastError, logs.String())
	}

	forwarder.fail = false
	if _, err = worker.DeliverReady(context.Background(), now.Add(2*time.Second)); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(forwarder.events) != 2 || forwarder.events[1].ID != eventID {
		t.Fatalf("retry did not carry the same idempotency identity: %#v", forwarder.events)
	}
	var remaining int
	if err = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_forwarding_outbox WHERE event_id = $1`, eventID).
		Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("successfully forwarded event remained in the outbox")
	}
}

func TestAuditForwardingTerminalFailureCanBeReplayed(t *testing.T) {
	database, organization := migratedDatabase(t)
	database.EnableAuditForwarding()
	_, err := database.CreateIntegration(context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			Type: integrations.TypeAlertmanager, Name: "terminal audit integration",
			WebhookSecretDigest: randomDigest(t), WebhookSecretFingerprint: "fingerprint",
		})
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &recordingAuditForwarder{fail: true, failure: errors.New("remote unavailable")}
	worker := audit.ForwardingWorker{
		Outbox: database, Forwarder: forwarder, Owner: "worker-terminal", Lease: time.Minute,
		RetryBase: time.Second, MaxAttempts: 2, Batch: 1, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	now := time.Now().UTC().Add(time.Second)
	if _, err = worker.DeliverReady(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.DeliverReady(context.Background(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	eventID := forwarder.events[0].ID
	pool, _ := database.Pool(organization)
	var terminal bool
	if err = pool.QueryRow(context.Background(), `
		SELECT terminal FROM audit_forwarding_outbox WHERE event_id = $1`, eventID).
		Scan(&terminal); err != nil || !terminal {
		t.Fatalf("terminal state = %t, %v", terminal, err)
	}
	if _, err = worker.DeliverReady(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(forwarder.events) != 2 {
		t.Fatalf("terminal event was retried without replay: %d calls", len(forwarder.events))
	}

	if err = database.ReplayAuditDelivery(context.Background(), organization,
		uuid.MustParse(eventID)); err != nil {
		t.Fatalf("replaying: %v", err)
	}
	forwarder.fail = false
	if _, err = worker.DeliverReady(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(forwarder.events) != 3 || forwarder.events[2].ID != eventID {
		t.Fatalf("replayed event = %#v", forwarder.events)
	}
}

func TestAuditOutboxFailureRollsBackTheMutation(t *testing.T) {
	database, organization := migratedDatabase(t)
	database.EnableAuditForwarding()
	pool, _ := database.Pool(organization)
	if _, err := pool.Exec(context.Background(), `DROP TABLE audit_forwarding_outbox`); err != nil {
		t.Fatal(err)
	}

	_, err := database.CreateIntegration(context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			Type: integrations.TypeAlertmanager, Name: "must roll back",
			WebhookSecretDigest: randomDigest(t), WebhookSecretFingerprint: "fingerprint",
		})
	if !errors.Is(err, audit.ErrWriteFailed) {
		t.Fatalf("mutation error = %v, want audit write failure", err)
	}
	var count int
	if queryErr := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM integration WHERE org_id = $1 AND name = 'must roll back'`,
		organization.String()).Scan(&count); queryErr != nil {
		t.Fatal(queryErr)
	}
	if count != 0 {
		t.Fatal("mutation committed without its forwarding outbox record")
	}

	standaloneTarget := uuid.NewString()
	err = database.RecordEvent(context.Background(), organization, audit.Event{
		Organization: organization.String(), Actor: audit.System("test"),
		Action:  audit.ActionAuthorizationRefused,
		Target:  audit.Target{Kind: audit.TargetRoute, ID: standaloneTarget},
		Outcome: audit.OutcomeDenied,
	})
	if !errors.Is(err, audit.ErrWriteFailed) {
		t.Fatalf("standalone event error = %v, want audit write failure", err)
	}
	if queryErr := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_event WHERE org_id = $1 AND target_id = $2`,
		organization.String(), standaloneTarget).Scan(&count); queryErr != nil {
		t.Fatal(queryErr)
	}
	if count != 0 {
		t.Fatal("standalone Audit Event committed without its forwarding outbox record")
	}
}

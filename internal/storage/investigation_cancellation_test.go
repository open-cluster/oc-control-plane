package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

func TestInvestigationCancellationIsTerminalAttributedAndAudited(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	opened := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, opened.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization, opened.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening the investigation: took=%v error=%v", took, err)
	}
	principal := ownerOf(t, organization)
	ended, err := database.CancelInvestigation(context.Background(), principal, organization, turn.InvestigationID)
	if err != nil {
		t.Fatalf("cancelling the investigation: %v", err)
	}
	if ended.Status != investigation.StatusCancelled {
		t.Fatalf("investigation status = %s, want cancelled", ended.Status)
	}
	activity, err := database.Events(context.Background(), organization,
		turn.InvestigationID, 0, 10)
	if err != nil {
		t.Fatalf("reading the cancelled investigation's activity: %v", err)
	}
	if len(activity) != 1 || activity[0].Type != investigation.EventCancelled ||
		!activity[0].Type.Terminal() {
		t.Fatalf("cancellation produced no explicit terminal semantic event: %+v", activity)
	}
	if activity[0].Payload["message"] != "Investigation cancelled by an operator" {
		t.Fatalf("terminal cancellation activity is not safe and explicit: %+v", activity[0])
	}
	if err = database.AppendEvent(context.Background(), organization, turn.InvestigationID,
		investigation.Event{Sequence: 2, At: time.Now().UTC(),
			Type: investigation.EventProgress, Payload: map[string]any{"message": "too late"}}); !errors.Is(err, investigation.ErrAlreadyEnded) {
		t.Fatalf("a remote worker appended activity after the terminal cancellation: %v", err)
	}
	activity, err = database.Events(context.Background(), organization, turn.InvestigationID, 0, 10)
	if err != nil || len(activity) != 1 || !activity[len(activity)-1].Type.Terminal() {
		t.Fatalf("the cancellation stream is not terminal: %+v, %v", activity, err)
	}
	if _, err = database.CancelInvestigation(context.Background(), principal, organization,
		turn.InvestigationID); !errors.Is(err, investigation.ErrAlreadyEnded) {
		t.Fatalf("cancelling a terminal investigation = %v, want an explicit refusal", err)
	}
	events, err := database.AuditEvents(context.Background(), principal, organization, audit.Page{})
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	for _, event := range events.Events {
		if event.Action == audit.ActionInvestigationCancelled {
			return
		}
	}
	t.Fatal("cancelling an investigation produced no atomic audit event")
}

func TestInvestigationCancellationDurablyStopsPendingAndExecutingRelayJobs(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)
	conversation := openConversation(t, database, organization, "cancel cluster work")
	say(t, database, organization, conversation.ID, "stop the active reads")
	turn, opened, err := database.OpenTurn(context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !opened {
		t.Fatalf("opening the investigation: opened=%v error=%v", opened, err)
	}

	newJob := func() uuid.UUID {
		t.Helper()
		job := storage.Job{
			ID: uuid.New(), InvestigationID: turn.InvestigationID,
			IntegrationID: integration, RegistrationID: registration,
			CapabilityID: "kubernetes.workload.runtime", CapabilityVersion: 1,
			Arguments: []byte("bounded"),
		}
		if _, err := database.EnqueueJob(context.Background(), organization, job); err != nil {
			t.Fatalf("enqueueing investigation-owned Relay work: %v", err)
		}
		return job.ID
	}
	running := newJob()
	session := uuid.New()
	claim(t, database, organization, registration, session)
	pending := newJob()

	if _, err = database.CancelInvestigation(context.Background(), ownerOf(t, organization),
		organization, turn.InvestigationID); err != nil {
		t.Fatalf("cancelling investigation-owned Relay work: %v", err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		id   uuid.UUID
		want storage.JobStatus
	}{
		{name: "executing", id: running, want: storage.JobLeased},
		{name: "pending", id: pending, want: storage.JobCancelled},
	} {
		var status storage.JobStatus
		var requested bool
		if err := pool.QueryRow(context.Background(), `
			SELECT status, cancel_requested_at IS NOT NULL
			  FROM relay_job WHERE org_id = $1 AND job_id = $2`,
			organization.String(), scenario.id).Scan(&status, &requested); err != nil {
			t.Fatalf("reading %s Relay work: %v", scenario.name, err)
		}
		if status != scenario.want || !requested {
			t.Errorf("%s Relay work status=%d cancellation_requested=%v, want status=%d and a durable stop",
				scenario.name, status, requested, scenario.want)
		}
	}
	cancellations, err := database.PendingCancellations(context.Background(), organization, session)
	if err != nil || len(cancellations) != 1 || cancellations[0].JobID != running {
		t.Fatalf("the real Relay session cannot observe its durable cancellation: %+v, %v",
			cancellations, err)
	}
}

func TestInvestigationCancellationCannotRacePastAConcurrentRelayJob(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)
	conversation := openConversation(t, database, organization, "cancel concurrent cluster work")
	say(t, database, organization, conversation.ID, "stop the concurrent read")
	turn, opened, err := database.OpenTurn(context.Background(), organization, conversation.ID,
		turnWindowLead)
	if err != nil || !opened {
		t.Fatalf("opening the investigation: opened=%v error=%v", opened, err)
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `
		CREATE FUNCTION hold_investigation_job() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(82734019); RETURN NEW; END $$;
		CREATE TRIGGER hold_investigation_job BEFORE INSERT ON relay_job
		FOR EACH ROW EXECUTE FUNCTION hold_investigation_job()`); err != nil {
		t.Fatalf("installing the deterministic concurrent-insert barrier: %v", err)
	}
	barrier, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Release()
	if _, err = barrier.Exec(context.Background(), `SELECT pg_advisory_lock(82734019)`); err != nil {
		t.Fatalf("holding the concurrent-insert barrier: %v", err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			_, _ = barrier.Exec(context.Background(), `SELECT pg_advisory_unlock(82734019)`)
		}
	}()
	job := storage.Job{ID: uuid.New(), InvestigationID: turn.InvestigationID,
		IntegrationID: integration, RegistrationID: registration,
		CapabilityID: "kubernetes.workload.runtime", CapabilityVersion: 1,
		Arguments: []byte("bounded")}
	enqueued := make(chan error, 1)
	go func() {
		_, enqueueError := database.EnqueueJob(context.Background(), organization, job)
		enqueued <- enqueueError
	}()
	deadline := time.After(10 * time.Second)
	for {
		var waiting bool
		err = pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'
			                  AND query LIKE '%INSERT INTO relay_job%')`).Scan(&waiting)
		if err != nil {
			t.Fatalf("observing the blocked concurrent insert: %v", err)
		}
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the concurrent Relay insert never reached its deterministic barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
	principal := ownerOf(t, organization)
	cancelled := make(chan error, 1)
	go func() {
		_, cancelError := database.CancelInvestigation(context.Background(), principal,
			organization, turn.InvestigationID)
		cancelled <- cancelError
	}()
	cancelCompleted := false
	select {
	case err = <-cancelled:
		cancelCompleted = true
		t.Errorf("cancellation raced past an investigation-owned insert: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err = barrier.Exec(context.Background(), `SELECT pg_advisory_unlock(82734019)`); err != nil {
		t.Fatalf("releasing the concurrent-insert barrier: %v", err)
	}
	unlocked = true
	if err = <-enqueued; err != nil {
		t.Fatalf("enqueueing the synchronized Relay job: %v", err)
	}
	if !cancelCompleted {
		err = <-cancelled
		if err != nil {
			t.Fatalf("cancelling synchronized Relay work: %v", err)
		}
	}
	var status storage.JobStatus
	if err = pool.QueryRow(context.Background(),
		`SELECT status FROM relay_job WHERE org_id = $1 AND job_id = $2`,
		organization.String(), job.ID).Scan(&status); err != nil {
		t.Fatalf("reading synchronized Relay work: %v", err)
	}
	if status != storage.JobCancelled {
		t.Fatalf("concurrent Relay work survived cancellation with status %d", status)
	}
}

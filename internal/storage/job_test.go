package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The guarantee under test is narrow and absolute: a job is never lost and never silently
// completed twice. Each test below is one way that could break.
//
// Every assertion is about durable state — what the job reached, what was recorded, what was
// refused — rather than about which calls were made. Only the database proves the guarantee
// held; a sequence of calls proves a conversation happened.

const testOrganization = "org-a"

func TestJob_ClaimingLeasesTheWorkAndFencesIt(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()
	job := enqueue(t, placements, organization, registration)

	claimed := claim(t, placements, organization, registration, uuid.New())

	if len(claimed) != 1 || claimed[0].ID != job {
		t.Fatalf("claimed %d jobs, want the one enqueued", len(claimed))
	}
	if claimed[0].LeaseEpoch != 1 {
		t.Errorf("first lease is generation %d, want 1", claimed[0].LeaseEpoch)
	}
}

func TestJob_ResultIsRecordedOnceAndResendsAreAnsweredDefinitively(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()
	enqueue(t, placements, organization, registration)
	session := uuid.New()
	leased := claim(t, placements, organization, registration, session)[0]

	fence := storage.JobFence{
		JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
	}
	outcome := storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("the-result")}

	if _, err := placements.RecordResult(context.Background(), organization, fence, outcome); err != nil {
		t.Fatalf("recording the result: %v", err)
	}

	// A relay that never saw the acknowledgement resends. It must be told the outcome is
	// already recorded, so its buffer drains rather than growing.
	refusal, err := placements.RecordResult(context.Background(), organization, fence, outcome)
	if !errors.Is(err, storage.ErrResultRefused) {
		t.Fatalf("a resend returned %v, want a refusal", err)
	}
	if refusal != storage.ResultAlreadyRecorded {
		t.Errorf("a resend was refused as %v, want already recorded", refusal)
	}
}

// A result produced by an execution that has since lost its lease must not overwrite the
// outcome of the execution that owns the work now. This is the failure a healthy run never
// produces and the reason the fence exists.
func TestJob_ResultUnderASupersededLeaseIsRefused(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()
	enqueue(t, placements, organization, registration)

	first := claim(t, placements, organization, registration, uuid.New())[0]
	expireLease(t, placements, organization, first.ID)
	second := claim(t, placements, organization, registration, uuid.New())[0]

	if second.LeaseEpoch <= first.LeaseEpoch {
		t.Fatalf("reclaiming produced generation %d, which does not supersede %d",
			second.LeaseEpoch, first.LeaseEpoch)
	}

	stale := storage.JobFence{
		JobID: first.ID, LeaseSession: first.LeaseSession, LeaseEpoch: first.LeaseEpoch,
	}
	refusal, err := placements.RecordResult(context.Background(), organization, stale,
		storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("stale")})
	if !errors.Is(err, storage.ErrResultRefused) {
		t.Fatalf("a superseded result returned %v, want a refusal", err)
	}
	if refusal != storage.ResultFenceSuperseded {
		t.Errorf("refused as %v, want lease superseded", refusal)
	}

	// The current execution must still be able to record, which is the point of refusing
	// the older one rather than treating the job as finished.
	current := storage.JobFence{
		JobID: second.ID, LeaseSession: second.LeaseSession, LeaseEpoch: second.LeaseEpoch,
	}
	if _, err = placements.RecordResult(context.Background(), organization, current,
		storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("current")}); err != nil {
		t.Fatalf("the owning execution could not record: %v", err)
	}
}

func TestJob_ExpiredLeaseReturnsToPendingAndTerminalWorkIsNeverSwept(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	// Both jobs are claimed together, then one is finished and the other abandoned. Claiming
	// them separately would not work: a claim reclaims expired leases itself, so the second
	// call would take the abandoned job back before the sweep could see it.
	enqueue(t, placements, organization, registration)
	enqueue(t, placements, organization, registration)
	leased := claim(t, placements, organization, registration, uuid.New())
	if len(leased) != 2 {
		t.Fatalf("claimed %d jobs, want both", len(leased))
	}
	abandoned, done := leased[0], leased[1]

	if _, err := placements.RecordResult(context.Background(), organization,
		storage.JobFence{JobID: done.ID, LeaseSession: done.LeaseSession, LeaseEpoch: done.LeaseEpoch},
		storage.JobOutcome{Status: storage.JobSucceeded}); err != nil {
		t.Fatalf("recording the finished job: %v", err)
	}
	expireLease(t, placements, organization, abandoned.ID)

	swept, err := placements.SweepExpiredLeases(context.Background(), organization)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept %d jobs, want exactly the abandoned one — a terminal job re-run is "+
			"the duplicate execution the fence exists to prevent", swept)
	}

	reclaimed := claim(t, placements, organization, registration, uuid.New())
	if len(reclaimed) != 1 || reclaimed[0].ID != abandoned.ID {
		t.Fatalf("after sweeping, claimed %d jobs, want the abandoned one back", len(reclaimed))
	}
}

// Work enqueued while nothing is connected must still be there when a session arrives. An
// outage delays investigation; it must not lose it.
func TestJob_WorkEnqueuedWithNoSessionIsDeliveredOnTheNextClaim(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	first := enqueue(t, placements, organization, registration)
	second := enqueue(t, placements, organization, registration)

	claimed := claim(t, placements, organization, registration, uuid.New())
	if len(claimed) != 2 {
		t.Fatalf("claimed %d jobs, want both %v and %v", len(claimed), first, second)
	}
}

func migratedPlacement(t *testing.T) (*storage.Placements, tenancy.Organization) {
	t.Helper()

	placements := openPlacements(t,
		map[string]string{"shared": postgresDSN(t)},
		map[string]string{testOrganization: "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	organization, err := tenancy.NewOrganization(testOrganization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return placements, organization
}

func enqueue(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, registration uuid.UUID,
) uuid.UUID {
	t.Helper()

	job := storage.Job{
		ID:                uuid.New(),
		RegistrationID:    registration,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	}
	if err := placements.EnqueueJob(context.Background(), organization, job); err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	return job.ID
}

func claim(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, registration, session uuid.UUID,
) []storage.Job {
	t.Helper()

	claimed, err := placements.ClaimJobs(context.Background(), organization, storage.JobClaim{
		RegistrationID: registration,
		SessionID:      session,
		LeaseFor:       time.Minute,
		Limit:          10,
	})
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("claimed nothing, want at least one job")
	}
	return claimed
}

// expireLease moves a lease's deadline into the past. Expiry is induced rather than waited
// for: a suite that depends on winning a timing race is a suite that gets disabled.
func expireLease(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, job uuid.UUID,
) {
	t.Helper()

	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("resolving the placement: %v", err)
	}
	if _, err = pool.Exec(context.Background(),
		`UPDATE relay_job SET lease_expires_at = now() - interval '1 minute' WHERE job_id = $1`,
		job); err != nil {
		t.Fatalf("expiring the lease: %v", err)
	}
}

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

// Losing the fence is not the same as being superseded, and the difference decides whether a
// finished execution's result survives. A relay told it is superseded drops the result it is
// holding, so saying that when nothing has taken the job over throws away work no other
// execution is going to redo.
func TestJob_ResultWhoseLeaseMovedIsNotCalledSuperseded(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()
	enqueue(t, placements, organization, registration)
	leased := claim(t, placements, organization, registration, uuid.New())[0]

	// The job is still at the generation this execution was given; only the session holding it
	// differs, which is what a reconnection without adoption looks like.
	elsewhere := storage.JobFence{
		JobID: leased.ID, LeaseSession: uuid.New(), LeaseEpoch: leased.LeaseEpoch,
	}
	refusal, err := placements.RecordResult(context.Background(), organization, elsewhere,
		storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("finished anyway")})
	if !errors.Is(err, storage.ErrResultRefused) {
		t.Fatalf("recording under a lease held elsewhere returned %v, want a refusal", err)
	}
	if refusal != storage.ResultLeaseNotHeld {
		t.Errorf("refused as %v, want the lease reported as held elsewhere — calling this a "+
			"supersession tells the relay to discard a result nothing else will produce",
			refusal)
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

// Asking a job to stop means three different things depending on where the job has got to,
// and conflating them either loses an outcome that already happened or leaves a job that
// nothing will ever finish.
func TestJob_CancellationDependsOnWhetherTheJobHasStarted(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	t.Run("a job that has not started is cancelled outright", func(t *testing.T) {
		job := enqueue(t, placements, organization, registration)

		outcome, err := placements.RequestJobCancellation(context.Background(), organization, job)
		if err != nil {
			t.Fatalf("cancelling: %v", err)
		}
		if outcome != storage.CancellationRecorded {
			t.Fatalf("cancelling an unstarted job gave %v, want it recorded — no relay is "+
				"executing it, so nothing else can ever finish it", outcome)
		}

		// It must not then be handed to a relay: it is already over.
		claimed, err := placements.ClaimJobs(context.Background(), organization, storage.JobClaim{
			RegistrationID: registration, SessionID: uuid.New(),
			LeaseFor: time.Minute, Capacity: 10,
		})
		if err != nil {
			t.Fatalf("claiming: %v", err)
		}
		if len(claimed) != 0 {
			t.Errorf("a cancelled job was dispatched to a relay")
		}
	})

	t.Run("a job that is executing is asked to stop and stays live", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]

		outcome, err := placements.RequestJobCancellation(
			context.Background(), organization, leased.ID)
		if err != nil {
			t.Fatalf("cancelling: %v", err)
		}
		if outcome != storage.CancellationRequested {
			t.Fatalf("cancelling an executing job gave %v, want it requested", outcome)
		}

		// The request is advisory, so the relay's report of what actually happened must still
		// be recordable. Anything else would decide the outcome of an execution it cannot see.
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err = placements.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobFailed}); err != nil {
			t.Fatalf("the executing relay could not record its outcome: %v", err)
		}
	})

	t.Run("a job that has finished cannot be cancelled", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := placements.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("done")}); err != nil {
			t.Fatalf("recording: %v", err)
		}

		outcome, err := placements.RequestJobCancellation(
			context.Background(), organization, leased.ID)
		if err != nil {
			t.Fatalf("cancelling: %v", err)
		}
		if outcome != storage.CancellationRefused {
			t.Errorf("cancelling a finished job gave %v, want it refused — a result that "+
				"already exists is not undone by asking for a stop", outcome)
		}
	})
}

// The session sends a cancellation to the relay executing the job, so the request has to be
// findable from the session that holds the lease.
func TestJob_PendingCancellationsAreScopedToTheHoldingSession(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	enqueue(t, placements, organization, registration)
	enqueue(t, placements, organization, registration)
	session := uuid.New()
	leased := claim(t, placements, organization, registration, session)
	asked, untouched := leased[0], leased[1]

	if _, err := placements.RequestJobCancellation(
		context.Background(), organization, asked.ID); err != nil {
		t.Fatalf("cancelling: %v", err)
	}

	pending, err := placements.PendingCancellations(context.Background(), organization, session)
	if err != nil {
		t.Fatalf("reading pending cancellations: %v", err)
	}
	if len(pending) != 1 || pending[0].JobID != asked.ID {
		t.Fatalf("found %d cancellations, want exactly the one asked for (not %v)",
			len(pending), untouched.ID)
	}
	if pending[0].LeaseEpoch != asked.LeaseEpoch || pending[0].LeaseSession != session {
		t.Error("the cancellation carries a different fence from the lease it belongs to; " +
			"a relay could not tell which execution it was being asked to stop")
	}

	// Another session must not be told to stop work it is not executing.
	other, err := placements.PendingCancellations(context.Background(), organization, uuid.New())
	if err != nil {
		t.Fatalf("reading pending cancellations: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a session was handed %d cancellations for work it does not hold", len(other))
	}
}

// A relay that reconnects mid-execution is still holding the result of work it was legitimately
// assigned. Adoption is what lets that result still be recorded — and it is the one place a
// relay's own account of the world is allowed to change durable state, so what it CANNOT do
// matters more than what it can.
func TestJob_LeaseAdoptionRenewsOnlyWhatTheRelayAlreadyHeld(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	t.Run("work still executing moves to the new session and can still be recorded", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]

		reconnected := uuid.New()
		adopt(t, placements, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      reconnected,
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 1)

		// The generation must be unchanged. Raising it would invalidate the very result the
		// relay is holding, which is the thing adoption exists to preserve.
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: reconnected, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := placements.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("carried over")},
		); err != nil {
			t.Fatalf("the reconnected relay could not record work it never stopped executing: %v", err)
		}
	})

	t.Run("a generation the relay does not hold adopts nothing", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]

		// Naming a later generation is how a relay would claim an execution that superseded
		// its own. The fence decides, not the declaration.
		adopt(t, placements, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch + 1},
			},
		}, 0)
	})

	t.Run("another registration's work is untouchable", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]

		adopt(t, placements, organization, storage.LeaseAdoption{
			RegistrationID: uuid.New(),
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 0)
	})

	t.Run("finished work is not revived", func(t *testing.T) {
		enqueue(t, placements, organization, registration)
		leased := claim(t, placements, organization, registration, uuid.New())[0]
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := placements.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded}); err != nil {
			t.Fatalf("recording: %v", err)
		}

		// A job whose outcome exists must not be reopened by a relay that still thinks it is
		// running it — that would put a finished job back on the wire.
		adopt(t, placements, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 0)
	})

	t.Run("work that was never enqueued cannot be invented", func(t *testing.T) {
		adopt(t, placements, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight:       []storage.InFlightJob{{JobID: uuid.New(), LeaseEpoch: 1}},
		}, 0)
	})
}

// Once a reconnected relay has said what it is still executing, whatever else it holds belongs
// to a session that is gone. Leaving that work leased adds a full lease of doing nothing to the
// far side of every network blip.
func TestJob_LeasesNoRelayIsExecutingAreReleasedAtOnce(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := uuid.New()

	enqueue(t, placements, organization, registration)
	enqueue(t, placements, organization, registration)
	enqueue(t, placements, organization, registration)
	leased := claim(t, placements, organization, registration, uuid.New())
	if len(leased) != 3 {
		t.Fatalf("claimed %d jobs, want three", len(leased))
	}
	stillRunning, abandoned, finished := leased[0], leased[1], leased[2]

	if _, err := placements.RecordResult(context.Background(), organization,
		storage.JobFence{
			JobID:        finished.ID,
			LeaseSession: finished.LeaseSession,
			LeaseEpoch:   finished.LeaseEpoch,
		},
		storage.JobOutcome{Status: storage.JobSucceeded}); err != nil {
		t.Fatalf("recording the finished job: %v", err)
	}

	reconnected := uuid.New()
	adopt(t, placements, organization, storage.LeaseAdoption{
		RegistrationID: registration,
		SessionID:      reconnected,
		LeaseFor:       time.Minute,
		InFlight: []storage.InFlightJob{
			{JobID: stillRunning.ID, LeaseEpoch: stillRunning.LeaseEpoch},
		},
	}, 1)

	released, err := placements.ReleaseStrandedLeases(
		context.Background(), organization, registration, reconnected)
	if err != nil {
		t.Fatalf("releasing stranded leases: %v", err)
	}
	if released != 1 {
		t.Fatalf("released %d jobs, want only the abandoned one — the adopted job is being "+
			"executed and the finished one is over", released)
	}

	// The abandoned job comes straight back; the adopted one stays with the relay running it.
	reclaimed := claim(t, placements, organization, registration, reconnected)
	if len(reclaimed) != 1 || reclaimed[0].ID != abandoned.ID {
		t.Fatalf("reclaimed %d jobs, want the abandoned one back at once", len(reclaimed))
	}
}

// adopt runs an adoption and asserts how much of it was accepted.
func adopt(
	t *testing.T,
	placements *storage.Placements,
	organization tenancy.Organization,
	adoption storage.LeaseAdoption,
	want int,
) {
	t.Helper()

	adopted, err := placements.AdoptInFlightLeases(context.Background(), organization, adoption)
	if err != nil {
		t.Fatalf("adopting in-flight leases: %v", err)
	}
	if len(adopted) != want {
		t.Fatalf("adopted %d of %d declared jobs, want %d",
			len(adopted), len(adoption.InFlight), want)
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
		Capacity:       10,
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

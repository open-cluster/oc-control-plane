package storage_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const testOrganization = "11111111-1111-4111-8111-111111111111"

func TestJob_ClaimingLeasesTheWorkAndFencesIt(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	job := enqueue(t, database, organization, registration)

	claimed := claim(t, database, organization, registration, uuid.New())

	if len(claimed) != 1 || claimed[0].ID != job {
		t.Fatalf("claimed %d jobs, want the one enqueued", len(claimed))
	}
	if claimed[0].LeaseEpoch != 1 {
		t.Errorf("first lease is generation %d, want 1", claimed[0].LeaseEpoch)
	}
}

func TestJob_ResultIsRecordedOnceAndResendsAreAnsweredDefinitively(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	enqueue(t, database, organization, registration)
	session := uuid.New()
	leased := claim(t, database, organization, registration, session)[0]

	fence := storage.JobFence{
		JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
	}
	outcome := storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("the-result")}

	if _, err := database.RecordResult(context.Background(), organization, fence, outcome); err != nil {
		t.Fatalf("recording the result: %v", err)
	}

	refusal, err := database.RecordResult(context.Background(), organization, fence, outcome)
	if !errors.Is(err, storage.ErrResultRefused) {
		t.Fatalf("a resend returned %v, want a refusal", err)
	}
	if refusal != storage.ResultAlreadyRecorded {
		t.Errorf("a resend was refused as %v, want already recorded", refusal)
	}
}

func TestJob_ResultUnderASupersededLeaseIsRefused(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	enqueue(t, database, organization, registration)

	first := claim(t, database, organization, registration, uuid.New())[0]
	expireLease(t, database, organization, first.ID)
	second := claim(t, database, organization, registration, uuid.New())[0]

	if second.LeaseEpoch <= first.LeaseEpoch {
		t.Fatalf("reclaiming produced generation %d, which does not supersede %d",
			second.LeaseEpoch, first.LeaseEpoch)
	}

	stale := storage.JobFence{
		JobID: first.ID, LeaseSession: first.LeaseSession, LeaseEpoch: first.LeaseEpoch,
	}
	refusal, err := database.RecordResult(context.Background(), organization, stale,
		storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("stale")})
	if !errors.Is(err, storage.ErrResultRefused) {
		t.Fatalf("a superseded result returned %v, want a refusal", err)
	}
	if refusal != storage.ResultFenceSuperseded {
		t.Errorf("refused as %v, want lease superseded", refusal)
	}

	current := storage.JobFence{
		JobID: second.ID, LeaseSession: second.LeaseSession, LeaseEpoch: second.LeaseEpoch,
	}
	if _, err = database.RecordResult(context.Background(), organization, current,
		storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("current")}); err != nil {
		t.Fatalf("the owning execution could not record: %v", err)
	}
}

func TestJob_ResultWhoseLeaseMovedIsNotCalledSuperseded(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	enqueue(t, database, organization, registration)
	leased := claim(t, database, organization, registration, uuid.New())[0]

	elsewhere := storage.JobFence{
		JobID: leased.ID, LeaseSession: uuid.New(), LeaseEpoch: leased.LeaseEpoch,
	}
	refusal, err := database.RecordResult(context.Background(), organization, elsewhere,
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

func TestJob_WorkEnqueuedWithNoSessionIsDeliveredOnTheNextClaim(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)

	first := enqueue(t, database, organization, registration)
	second := enqueue(t, database, organization, registration)

	claimed := claim(t, database, organization, registration, uuid.New())
	if len(claimed) != 2 {
		t.Fatalf("claimed %d jobs, want both %v and %v", len(claimed), first, second)
	}
}

func TestJob_CancellationDependsOnWhetherTheJobHasStarted(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)

	t.Run("a job that has not started is cancelled outright", func(t *testing.T) {
		job := enqueue(t, database, organization, registration)

		outcome, err := database.RequestJobCancellation(context.Background(), organization, job)
		if err != nil {
			t.Fatalf("cancelling: %v", err)
		}
		if outcome != storage.CancellationRecorded {
			t.Fatalf("cancelling an unstarted job gave %v, want it recorded — no relay is "+
				"executing it, so nothing else can ever finish it", outcome)
		}

		claimed, err := database.ClaimJobs(context.Background(), organization, storage.JobClaim{
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
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]

		outcome, err := database.RequestJobCancellation(
			context.Background(), organization, leased.ID)
		if err != nil {
			t.Fatalf("cancelling: %v", err)
		}
		if outcome != storage.CancellationRequested {
			t.Fatalf("cancelling an executing job gave %v, want it requested", outcome)
		}

		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err = database.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobFailed}); err != nil {
			t.Fatalf("the executing relay could not record its outcome: %v", err)
		}
	})

	t.Run("a job that has finished cannot be cancelled", func(t *testing.T) {
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := database.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("done")}); err != nil {
			t.Fatalf("recording: %v", err)
		}

		outcome, err := database.RequestJobCancellation(
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

func TestJob_PendingCancellationsAreScopedToTheHoldingSession(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)

	enqueue(t, database, organization, registration)
	enqueue(t, database, organization, registration)
	session := uuid.New()
	leased := claim(t, database, organization, registration, session)
	asked, untouched := leased[0], leased[1]

	if _, err := database.RequestJobCancellation(
		context.Background(), organization, asked.ID); err != nil {
		t.Fatalf("cancelling: %v", err)
	}

	pending, err := database.PendingCancellations(context.Background(), organization, session)
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

	other, err := database.PendingCancellations(context.Background(), organization, uuid.New())
	if err != nil {
		t.Fatalf("reading pending cancellations: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a session was handed %d cancellations for work it does not hold", len(other))
	}
}

func TestJob_LeaseAdoptionRenewsOnlyWhatTheRelayAlreadyHeld(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)

	t.Run("work still executing moves to the new session and can still be recorded", func(t *testing.T) {
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]

		reconnected := uuid.New()
		adopt(t, database, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      reconnected,
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 1)

		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: reconnected, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := database.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded, Result: []byte("carried over")},
		); err != nil {
			t.Fatalf("the reconnected relay could not record work it never stopped executing: %v", err)
		}
	})

	t.Run("a generation the relay does not hold adopts nothing", func(t *testing.T) {
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]

		adopt(t, database, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch + 1},
			},
		}, 0)
	})

	t.Run("another registration's work is untouchable", func(t *testing.T) {
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]

		adopt(t, database, organization, storage.LeaseAdoption{
			RegistrationID: uuid.New(),
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 0)
	})

	t.Run("finished work is not revived", func(t *testing.T) {
		enqueue(t, database, organization, registration)
		leased := claim(t, database, organization, registration, uuid.New())[0]
		fence := storage.JobFence{
			JobID: leased.ID, LeaseSession: leased.LeaseSession, LeaseEpoch: leased.LeaseEpoch,
		}
		if _, err := database.RecordResult(context.Background(), organization, fence,
			storage.JobOutcome{Status: storage.JobSucceeded}); err != nil {
			t.Fatalf("recording: %v", err)
		}

		adopt(t, database, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight: []storage.InFlightJob{
				{JobID: leased.ID, LeaseEpoch: leased.LeaseEpoch},
			},
		}, 0)
	})

	t.Run("work that was never enqueued cannot be invented", func(t *testing.T) {
		adopt(t, database, organization, storage.LeaseAdoption{
			RegistrationID: registration,
			SessionID:      uuid.New(),
			LeaseFor:       time.Minute,
			InFlight:       []storage.InFlightJob{{JobID: uuid.New(), LeaseEpoch: 1}},
		}, 0)
	})
}

func TestJob_LeasesNoRelayIsExecutingAreReleasedAtOnce(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)

	enqueue(t, database, organization, registration)
	enqueue(t, database, organization, registration)
	enqueue(t, database, organization, registration)
	leased := claim(t, database, organization, registration, uuid.New())
	if len(leased) != 3 {
		t.Fatalf("claimed %d jobs, want three", len(leased))
	}
	stillRunning, abandoned, finished := leased[0], leased[1], leased[2]

	if _, err := database.RecordResult(context.Background(), organization,
		storage.JobFence{
			JobID:        finished.ID,
			LeaseSession: finished.LeaseSession,
			LeaseEpoch:   finished.LeaseEpoch,
		},
		storage.JobOutcome{Status: storage.JobSucceeded}); err != nil {
		t.Fatalf("recording the finished job: %v", err)
	}

	reconnected := uuid.New()
	adopt(t, database, organization, storage.LeaseAdoption{
		RegistrationID: registration,
		SessionID:      reconnected,
		LeaseFor:       time.Minute,
		InFlight: []storage.InFlightJob{
			{JobID: stillRunning.ID, LeaseEpoch: stillRunning.LeaseEpoch},
		},
	}, 1)

	released, err := database.ReleaseStrandedLeases(
		context.Background(), organization, registration, reconnected)
	if err != nil {
		t.Fatalf("releasing stranded leases: %v", err)
	}
	if released != 1 {
		t.Fatalf("released %d jobs, want only the abandoned one — the adopted job is being "+
			"executed and the finished one is over", released)
	}

	reclaimed := claim(t, database, organization, registration, reconnected)
	if len(reclaimed) != 1 || reclaimed[0].ID != abandoned.ID {
		t.Fatalf("reclaimed %d jobs, want the abandoned one back at once", len(reclaimed))
	}
}

func adopt(
	t *testing.T,
	database *storage.Database,
	organization uuid.UUID,
	adoption storage.LeaseAdoption,
	want int,
) {
	t.Helper()

	adopted, err := database.AdoptInFlightLeases(context.Background(), organization, adoption)
	if err != nil {
		t.Fatalf("adopting in-flight leases: %v", err)
	}
	if len(adopted) != want {
		t.Fatalf("adopted %d of %d declared jobs, want %d",
			len(adopted), len(adoption.InFlight), want)
	}
}

func migratedDatabase(t *testing.T) (*storage.Database, uuid.UUID) {
	t.Helper()

	database := openDatabaseForTest(t, postgresDSN(t))
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	organization := uuid.MustParse(testOrganization)
	ensureTestOrganization(t, database, organization)
	return database, organization
}

func ensureTestOrganization(
	t *testing.T, database *storage.Database, organization uuid.UUID,
) {
	t.Helper()
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatalf("opening organization pool: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO organization(org_id, display_name, created_by)
VALUES ($1, 'Test Organization', 'test') ON CONFLICT (org_id) DO NOTHING`, organization); err != nil {
		t.Fatalf("creating test organization: %v", err)
	}
}

func enqueue(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, registration uuid.UUID,
) uuid.UUID {
	t.Helper()
	return enqueueThrough(t, database, organization, registration,
		kubernetesIntegration(t, database, organization, registration))
}

func enqueueThrough(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, registration, integration uuid.UUID,
) uuid.UUID {
	t.Helper()

	job := storage.RelayJob{
		ID:                uuid.New(),
		IntegrationID:     integration,
		RegistrationID:    registration,
		CapabilityID:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
		Arguments:         []byte("arguments"),
	}
	err := database.EnqueueVerifiedJob(context.Background(), organization, job)
	if err != nil {
		t.Fatalf("enqueueing verified work: %v", err)
	}
	return job.ID
}

func enrolledRelay(
	t *testing.T, database *storage.Database, organization uuid.UUID,
) uuid.UUID {
	t.Helper()

	token := randomDigest(t)
	ctx := context.Background()
	if err := issueBootstrapTokenForTest(database,
		ctx, organization, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("issuing a bootstrap token: %v", err)
	}
	registration, refusal, err := database.EnrolRelay(ctx, organization, storage.RelayEnrolment{
		TokenDigest:        token,
		CredentialDigest:   randomDigest(t),
		ClusterFingerprint: uuid.NewString(),
		RelayVersion:       "test",
		Capabilities:       []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("enrolling a relay: %v (%v)", err, refusal)
	}
	return registration
}

func kubernetesIntegration(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, registration uuid.UUID,
) uuid.UUID {
	t.Helper()

	created, err := database.CreateIntegration(context.Background(), ownerOf(t, organization),
		organization, integrations.NewIntegration{
			Provider: "kubernetes",
			Name:     "cluster " + uuid.NewString(),
			RelayID:  registration,
		})
	if err != nil {
		t.Fatalf("creating a kubernetes integration: %v", err)
	}
	if _, err = database.RecordIntegrationVerification(context.Background(), ownerOf(t, organization),
		organization, created.ID, integrations.Verification{
			Status: integrations.StatusVerified,
			Grants: []string{"kubernetes.workload.runtime"},
		}); err != nil {
		t.Fatalf("verifying the kubernetes integration: %v", err)
	}
	return created.ID
}

func randomDigest(t *testing.T) []byte {
	t.Helper()
	digest := make([]byte, 32)
	if _, err := rand.Read(digest); err != nil {
		t.Fatalf("reading entropy: %v", err)
	}
	return digest
}

func claim(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, registration, session uuid.UUID,
) []storage.RelayJob {
	t.Helper()

	claimed, err := database.ClaimJobs(context.Background(), organization, storage.JobClaim{
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

func expireLease(
	t *testing.T, database *storage.Database,
	organization uuid.UUID, job uuid.UUID,
) {
	t.Helper()

	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatalf("resolving the database: %v", err)
	}
	if _, err = pool.Exec(context.Background(),
		`UPDATE relay_job SET lease_expires_at = now() - interval '1 minute' WHERE job_id = $1`,
		job); err != nil {
		t.Fatalf("expiring the lease: %v", err)
	}
}

func TestRelayLastSeenSortPagesRelaysThatHaveNeverConnected(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		_, err = pool.Exec(context.Background(), `
			INSERT INTO relay_registration
				(registration_id, org_id, credential_digest, cluster_fingerprint,
				 relay_version, capabilities, created_at)
			VALUES (gen_random_uuid(), $1, sha256($2::bytea), $3, '1.0.0', '[]',
			        now() + make_interval(secs => $4))`,
			organization, []byte(fmt.Sprintf("credential-%d", index)),
			fmt.Sprintf("relay-%d", index), float64(index))
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, descending := range []bool{false, true} {
		seen := map[string]bool{}
		cursor := ""
		for {
			page, listErr := database.ListRelays(context.Background(), ownerOf(t, organization),
				organization, storage.RelayQuery{
					Page: storage.Page{Limit: 1, After: cursor}, SortField: "lastSeenAt",
					Descending: descending, LivenessWindow: time.Minute,
				})
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, relay := range page.Relays {
				seen[relay.RegistrationID.String()] = true
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		if len(seen) != 3 {
			t.Errorf("descending=%v visited %d relays, want 3", descending, len(seen))
		}
	}
}

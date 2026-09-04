package storage_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// THE LEASE, WHERE IT HAS TO BE TRUE.
//
// Every property here is one that has to hold ACROSS processes: two workers claiming the
// same work, a worker that died leaving a record nobody would ever end, a renewal by a
// process that has already lost its claim. None of that can be observed from inside one
// process, so none of it is tested there.

func aClaim(worker string) investigation.Claim {
	return investigation.Claim{
		Worker: worker, LeaseFor: turnWindowLead,
	}
}

// expireInvestigationLease pushes a held lease into the past, standing in for the worker
// that stopped heartbeating because it stopped existing. Expiry is induced rather than
// waited for: a suite that depends on winning a timing race is a suite that gets disabled.
func expireInvestigationLease(
	t *testing.T, database *storage.Database, organization tenancy.Organization,
	id uuid.UUID,
) {
	t.Helper()

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if _, err = pool.Exec(context.Background(), `
		UPDATE investigation
		   SET lease_expires_at = now() - interval '1 minute'
		 WHERE investigation_id = $1`, id); err != nil {
		t.Fatalf("expiring a lease: %v", err)
	}
}

// TWO WORKERS, ONE DATABASE. Each investigation is claimed exactly once. This is what lets
// a deployment run more than one replica.
func TestEveryInvestigationIsClaimedExactlyOnce(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)

	const turns = 8
	opened := make(map[uuid.UUID]bool, turns)
	for range turns {
		conversation := openConversation(t, database, organization, "checkout is slow")
		say(t, database, organization, conversation.ID, "what changed?")
		turn, took, err := database.OpenTurn(context.Background(), organization,
			conversation.ID, turnWindowLead)
		if err != nil || !took {
			t.Fatalf("opening a turn: took=%v err=%v", took, err)
		}
		opened[turn.InvestigationID] = true
	}

	var (
		mutex   sync.Mutex
		claimed []uuid.UUID
		wait    sync.WaitGroup
	)
	for worker := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			name := "worker-" + string(rune('a'+worker))
			for {
				// A ceiling above the whole set, so what is being tested is the claim and
				// not the backpressure beside it.
				_, investigationRecord, took, err := database.ClaimInvestigation(
					context.Background(), aClaim(name))
				if err != nil {
					t.Errorf("%s: claiming: %v", name, err)
					return
				}
				if !took {
					return
				}
				mutex.Lock()
				claimed = append(claimed, investigationRecord.ID)
				mutex.Unlock()
			}
		}()
	}
	wait.Wait()

	if len(claimed) != turns {
		t.Fatalf("%d claims for %d turns; every one is claimed and none twice",
			len(claimed), turns)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range claimed {
		if seen[id] {
			t.Errorf("investigation %s was claimed twice", id)
		}
		seen[id] = true
		if !opened[id] {
			t.Errorf("investigation %s was claimed and never opened", id)
		}
	}
}

func TestInvestigationsAreClaimedOldestFirst(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	var opened []uuid.UUID
	for range 3 {
		conversation := openConversation(t, database, organization, "checkout is slow")
		say(t, database, organization, conversation.ID, "what changed?")
		turn, took, err := database.OpenTurn(context.Background(), organization,
			conversation.ID, turnWindowLead)
		if err != nil || !took {
			t.Fatalf("opening turn: took=%v err=%v", took, err)
		}
		opened = append(opened, turn.InvestigationID)
		time.Sleep(time.Millisecond)
	}
	for index, want := range opened {
		_, claimed, took, err := database.ClaimInvestigation(context.Background(), aClaim("worker"))
		if err != nil || !took {
			t.Fatalf("claim %d: took=%v err=%v", index, took, err)
		}
		if claimed.ID != want {
			t.Fatalf("claim %d = %s, want oldest %s", index, claimed.ID, want)
		}
	}
}

func TestHeartbeatIsFencedByWorkerIdentity(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if _, _, took, err = database.ClaimInvestigation(context.Background(), aClaim("worker-a")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	if held, heartbeatErr := database.Heartbeat(context.Background(), organization,
		turn.InvestigationID, aClaim("worker-b")); heartbeatErr != nil || held {
		t.Fatalf("another worker renewed the lease: held=%v err=%v", held, heartbeatErr)
	}
	if held, heartbeatErr := database.Heartbeat(context.Background(), organization,
		turn.InvestigationID, aClaim("worker-a")); heartbeatErr != nil || !held {
		t.Fatalf("the holder could not renew its lease: held=%v err=%v", held, heartbeatErr)
	}
}

// CRASH RECOVERY. A worker that stops heartbeating leaves an investigation that is FAILED
// with a stated reason and a terminal event — never left saying `running` forever, and
// never resumed, because there is nothing honest to resume from.
func TestALapsedLeaseFailsTheInvestigationAndEndsItsStream(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}

	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-that-dies")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	// The worker got as far as saying it had started, and then stopped existing.
	if err = database.AppendEvent(context.Background(), organization,
		turn.InvestigationID, investigation.Event{
			Sequence: 1, At: time.Now().UTC(), Type: investigation.EventStarted,
			Payload: map[string]any{"state": "executing"},
		}); err != nil {
		t.Fatalf("appending: %v", err)
	}
	expireInvestigationLease(t, database, organization, turn.InvestigationID)

	recovered, err := database.RecoverStale(context.Background(),
		investigation.RecoveryReason, 10)
	if err != nil {
		t.Fatalf("recovering: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("%d recovered, want one", recovered)
	}

	found, err := database.Investigation(context.Background(), organization,
		turn.InvestigationID)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if found.Status != investigation.StatusFailed {
		t.Errorf("status = %v, want failed; a worker that stopped must not leave a record "+
			"that says running forever", found.Status)
	}
	if found.Error != investigation.RecoveryReason {
		t.Errorf("error = %q, want the stated recovery reason", found.Error)
	}

	events, err := database.Events(context.Background(), organization,
		turn.InvestigationID, 0, 0)
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("%d events, want the started one and a terminal one: %+v", len(events),
			events)
	}
	last := events[len(events)-1]
	if last.Type != investigation.EventFailed {
		t.Errorf("the stream ended with %s, want failed; a reader watching a recovered "+
			"investigation must be told, not left on a spinner", last.Type)
	}
	if last.Sequence != 2 {
		t.Errorf("the terminal event is at sequence %d, want 2; the sequence continues "+
			"from the table because the process holding it in memory is gone",
			last.Sequence)
	}
	if last.Payload["reason"] != investigation.RecoveryReason {
		t.Errorf("the terminal event states %v, want the recovery reason",
			last.Payload["reason"])
	}
}

// A recovered investigation is not re-claimed. It ended; re-running it would be the
// duplicate execution the fence exists to prevent.
func TestARecoveredInvestigationIsNotClaimedAgain(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-that-dies")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	expireInvestigationLease(t, database, organization, turn.InvestigationID)
	if _, err = database.RecoverStale(context.Background(),
		investigation.RecoveryReason, 10); err != nil {
		t.Fatalf("recovering: %v", err)
	}

	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-b")); err != nil || took {
		t.Errorf("a recovered investigation was claimed again: took=%v err=%v", took, err)
	}
}

// Concluding releases the lease. A terminal investigation is nobody's to hold, and one
// left held would make the sweeper reason about work that is already finished.
func TestConcludingReleasesTheLease(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-a")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}

	if err = database.ConcludeInvestigation(context.Background(), organization,
		turn.InvestigationID, conclusionSaying("done"), "",
		investigation.Usage{}); err != nil {
		t.Fatalf("concluding: %v", err)
	}

	// Nothing to renew, and nothing for the sweeper to find.
	if held, heartbeatErr := database.Heartbeat(context.Background(), organization,
		turn.InvestigationID, aClaim("worker-a")); heartbeatErr != nil || held {
		t.Errorf("a concluded investigation still holds a lease: held=%v err=%v",
			held, heartbeatErr)
	}
	// The lease cannot even be pushed into the past to force the question: the schema's
	// own "a lease is a worker and an expiry together" constraint refuses a half lease, so
	// a concluded investigation with a lapsing lease is not a row that can exist. The
	// sweep is guarded on the status as well, and finds nothing here either way.
	recovered, err := database.RecoverStale(context.Background(),
		investigation.RecoveryReason, 10)
	if err != nil {
		t.Fatalf("recovering: %v", err)
	}
	if recovered != 0 {
		t.Errorf("%d concluded investigations were 'recovered'; a terminal one is already "+
			"recorded and re-running it is the duplicate the fence prevents", recovered)
	}
}

func TestTerminalInvestigationUsageRoundTrips(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	for _, test := range []struct {
		name string
		end  func(uuid.UUID, investigation.Usage) error
	}{
		{name: "concluded", end: func(id uuid.UUID, usage investigation.Usage) error {
			return database.ConcludeInvestigation(context.Background(), organization, id,
				conclusionSaying("done"), "", usage)
		}},
		{name: "failed", end: func(id uuid.UUID, usage investigation.Usage) error {
			return database.FailInvestigation(context.Background(), organization, id,
				"provider unavailable", usage)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversation := openConversation(t, database, organization, test.name)
			say(t, database, organization, conversation.ID, "what happened?")
			turn, took, err := database.OpenTurn(context.Background(), organization,
				conversation.ID, turnWindowLead)
			if err != nil || !took {
				t.Fatalf("opening a turn: took=%v err=%v", took, err)
			}
			want := investigation.Usage{InputTokens: 34, OutputTokens: 21}
			if err = test.end(turn.InvestigationID, want); err != nil {
				t.Fatal(err)
			}
			found, err := database.Investigation(context.Background(), organization,
				turn.InvestigationID)
			if err != nil {
				t.Fatal(err)
			}
			if found.Usage != want {
				t.Fatalf("usage=%+v, want %+v", found.Usage, want)
			}
		})
	}
}

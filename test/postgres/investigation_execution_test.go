package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func aTurn(
	t *testing.T, database *storage.Database, organization uuid.UUID,
) uuid.UUID {
	t.Helper()

	opened := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, opened.ID, "what changed?")
	turn, took, err := openTurnForTest(database, context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	return turn.InvestigationID
}

func appendEvents(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	id uuid.UUID, types ...investigation.EventType,
) {
	t.Helper()

	for position, eventType := range types {
		if err := database.AppendEvent(context.Background(), organization, id,
			claimToken(t, database, organization, id), investigation.Event{
				Sequence: int64(position + 1),
				At:       time.Now().UTC(),
				Type:     eventType,
				Payload:  map[string]any{"position": position},
			}); err != nil {
			t.Fatalf("appending event %d: %v", position+1, err)
		}
	}
}

func TestResumingFromASequenceProducesExactlyTheMissingSuffix(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)

	written := []investigation.EventType{
		investigation.EventStarted,
		investigation.EventProgress,
		investigation.EventToolStarted,
		investigation.EventToolCompleted,
		investigation.EventHypothesesUpdated,
		investigation.EventConcluded,
		investigation.EventFailed,
		investigation.EventCancelled,
	}
	appendEvents(t, database, organization, id, written...)

	for after := range len(written) + 1 {
		read, err := database.Events(context.Background(), organization, id,
			int64(after), 0)
		if err != nil {
			t.Fatalf("reading after %d: %v", after, err)
		}
		if len(read) != len(written)-after {
			t.Fatalf("after=%d returned %d events, want the %d that follow it",
				after, len(read), len(written)-after)
		}
		for position, event := range read {
			wantedSequence := int64(after + position + 1)
			if event.Sequence != wantedSequence {
				t.Errorf("after=%d event %d is at sequence %d, want %d", after, position,
					event.Sequence, wantedSequence)
			}
			if event.Type != written[after+position] {
				t.Errorf("after=%d event %d is %s, want %s", after, position, event.Type,
					written[after+position])
			}
		}
	}
}

func TestAnUnknownFutureEventDoesNotCorruptReadableHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx,
		`ALTER TABLE investigation_event DROP CONSTRAINT investigation_event_type_check`); err != nil {
		t.Fatalf("modeling a newer event schema: %v", err)
	}
	if _, err = pool.Exec(ctx, `
		INSERT INTO investigation_event
			(org_id, investigation_id, sequence, type, payload, at)
		VALUES ($1, $2, 1, 32000, '{"future":"preserved"}', now())`,
		organization, id); err != nil {
		t.Fatalf("seeding future history: %v", err)
	}
	if err = database.AppendEvent(ctx, organization, id, claimToken(t, database, organization, id), investigation.Event{
		Sequence: 2, At: time.Now().UTC(), Type: investigation.EventProgress,
		Payload: map[string]any{"text": "known history remains readable"},
	}); err != nil {
		t.Fatalf("appending known history after future event: %v", err)
	}
	events, err := database.Events(ctx, organization, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type.String() != "unrecognised" ||
		events[0].Payload["future"] != "preserved" ||
		events[1].Type != investigation.EventProgress {
		t.Fatalf("future event corrupted history: %+v", events)
	}
}

func TestReplaySkipsRetiredEventRowsWithoutRenumberingHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)
	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatal(err)
	}
	for sequence, eventType := range []int16{1, 5, 2, 8, 6} {
		if _, err = pool.Exec(ctx, `
			INSERT INTO investigation_event
				(org_id, investigation_id, sequence, type, payload, at)
			VALUES ($1, $2, $3, $4, '{}', now())`,
			organization, id, sequence+1, eventType); err != nil {
			t.Fatalf("seeding event %d: %v", sequence+1, err)
		}
	}

	events, err := database.Events(ctx, organization, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("replay returned %d active events, want 3: %+v", len(events), events)
	}
	wantSequences := []int64{1, 3, 5}
	wantTypes := []investigation.EventType{
		investigation.EventStarted, investigation.EventProgress, investigation.EventConcluded,
	}
	for index := range events {
		if events[index].Sequence != wantSequences[index] || events[index].Type != wantTypes[index] {
			t.Errorf("event %d = sequence %d type %s, want sequence %d type %s", index,
				events[index].Sequence, events[index].Type, wantSequences[index], wantTypes[index])
		}
	}
}

func TestAnEventPayloadSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)

	payload := map[string]any{
		"tool":      "slack.get_channel_history",
		"ordinal":   float64(3),
		"truncated": true,
		"arguments": map[string]any{"channel": "deploys"},
	}
	if err := database.AppendEvent(context.Background(), organization, id,
		claimToken(t, database, organization, id), investigation.Event{
			Sequence: 1, At: time.Now().UTC(),
			Type: investigation.EventToolStarted, Payload: payload,
		}); err != nil {
		t.Fatalf("appending: %v", err)
	}

	read, err := database.Events(context.Background(), organization, id, 0, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(read) != 1 {
		t.Fatalf("%d events, want one", len(read))
	}
	if read[0].Payload["tool"] != "slack.get_channel_history" ||
		read[0].Payload["ordinal"] != float64(3) ||
		read[0].Payload["truncated"] != true {
		t.Errorf("payload = %+v, want %+v", read[0].Payload, payload)
	}
	arguments, ok := read[0].Payload["arguments"].(map[string]any)
	if !ok || arguments["channel"] != "deploys" {
		t.Errorf("nested arguments did not survive: %+v", read[0].Payload["arguments"])
	}
}

func TestAnotherClaimCannotAppendProgress(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)

	first := investigation.Event{
		Sequence: 1, At: time.Now().UTC(), Type: investigation.EventStarted,
		Payload: map[string]any{"writer": "the lease holder"},
	}
	if err := database.AppendEvent(
		context.Background(), organization, id, claimToken(t, database, organization, id), first); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}

	second := first
	second.Payload = map[string]any{"writer": "somebody who should not be here"}
	if err := database.AppendEvent(
		context.Background(), organization, id, uuid.New(), second); err == nil {
		t.Fatal("another claim appended progress")
	}

	read, err := database.Events(context.Background(), organization, id, 0, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(read) != 1 || read[0].Payload["writer"] != "the lease holder" {
		t.Errorf("the refused write changed the stream: %+v", read)
	}
}

func TestInvestigationCannotBeDeletedThroughItsEventHistory(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)
	appendEvents(t, database, organization, id, investigation.EventStarted,
		investigation.EventConcluded)

	pool, err := poolForTest(database, organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if _, err = pool.Exec(context.Background(), `
		DELETE FROM conversation_message WHERE investigation_id = $1`, id); err != nil {
		t.Fatalf("detaching the turn's messages: %v", err)
	}
	if _, err = pool.Exec(context.Background(), `
		DELETE FROM investigation WHERE investigation_id = $1`, id); err == nil {
		t.Fatal("deleting an Investigation erased its replay history")
	}

	read, err := database.Events(context.Background(), organization, id, 0, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(read) != 2 {
		t.Errorf("refused Investigation deletion left %d events, want 2", len(read))
	}
}

func TestAnotherOrganizationsEventsAreNotReadable(t *testing.T) {
	t.Parallel()

	database, mine, theirs := twoOrganizationsInOneDatabase(t)
	id := aTurn(t, database, mine)
	appendEvents(t, database, mine, id, investigation.EventStarted,
		investigation.EventConcluded)

	read, err := database.Events(context.Background(), theirs, id, 0, 0)
	if err != nil {
		t.Fatalf("reading across tenants: %v", err)
	}
	if len(read) != 0 {
		t.Errorf("%d events readable from another organization", len(read))
	}
}

func TestExpiredWorkerCannotConclude(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	if err := database.ConcludeInvestigation(ctx, org, id, claimToken(t, database, org, id), conclusionSaying("late answer"), "", investigation.Usage{}); err == nil {
		t.Fatal("expired worker committed a conclusion")
	}
	found, err := database.Investigation(ctx, org, id)
	if err != nil || found.Status != investigation.StatusRunning {
		t.Fatalf("rejected conclusion changed state: status=%v err=%v", found.Status, err)
	}
}

func TestEveryWorkerWriteRequiresItsOwnClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	aTurn(t, database, org)
	aTurn(t, database, org)
	_, first, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker"))
	if err != nil || !took {
		t.Fatalf("first claim: %v %v", took, err)
	}
	_, second, took, err := database.ClaimInvestigation(ctx, aClaim("same-worker"))
	if err != nil || !took || first.ClaimToken == uuid.Nil || first.ClaimToken == second.ClaimToken {
		t.Fatalf("claims are not unique: %v %v", took, err)
	}
	now := time.Now().UTC()
	run := investigation.ToolRun{Ordinal: 1, Tool: "read", Purpose: "inspect", Outcome: investigation.RunSucceeded,
		WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now, FinishedAt: now}
	writes := map[string]func(uuid.UUID) error{
		"conclusion": func(token uuid.UUID) error {
			return database.ConcludeInvestigation(ctx, org, first.ID, token, conclusionSaying("answer"), "", investigation.Usage{})
		},
		"failure": func(token uuid.UUID) error {
			return database.FailInvestigation(ctx, org, first.ID, token, "failure", investigation.Usage{})
		},
		"tool run": func(token uuid.UUID) error { return database.RecordToolRun(ctx, org, first.ID, token, run) },
		"progress": func(token uuid.UUID) error {
			return database.AppendEvent(ctx, org, first.ID, token, investigation.Event{At: now, Type: investigation.EventProgress})
		},
	}
	for _, token := range []uuid.UUID{uuid.Nil, uuid.New(), second.ClaimToken} {
		for name, write := range writes {
			if err := write(token); err == nil {
				t.Errorf("%s accepted another claim", name)
			}
		}
		if held, err := database.Heartbeat(ctx, org, first.ID, investigation.Claim{Worker: "same-worker", LeaseFor: turnWindowLead, Token: token}); err != nil || held {
			t.Errorf("wrong claim heartbeat: %v %v", held, err)
		}
	}
	if events, err := database.Events(ctx, org, first.ID, 0, 0); err != nil || len(events) != 0 {
		t.Fatalf("rejected writes left events: %v %v", events, err)
	}
	for _, name := range []string{"progress", "tool run", "conclusion"} {
		if err := writes[name](first.ClaimToken); err != nil {
			t.Fatalf("valid %s: %v", name, err)
		}
	}
	for name, write := range writes {
		if err := write(first.ClaimToken); err == nil {
			t.Errorf("terminal claim accepted %s", name)
		}
	}
	events, err := database.Events(ctx, org, first.ID, 0, 0)
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || !events[1].Type.Terminal() {
		t.Fatalf("canonical ending: %+v %v", events, err)
	}
}

func TestConcurrentClaimWritesReceiveDurableSequences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	_, opened, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker"))
	if err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := database.AppendEvent(ctx, org, id, opened.ClaimToken, investigation.Event{Sequence: 1, At: time.Now(), Type: investigation.EventProgress}); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	workers.Wait()
	events, err := database.Events(ctx, org, id, 0, 0)
	if err != nil || len(events) != 10 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("position %d has sequence %d", index, event.Sequence)
		}
	}
}

func TestExpiredWorkerCannotRecordToolRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	now := time.Now().UTC()
	run := investigation.ToolRun{Ordinal: 1, Tool: "read", Purpose: "inspect", Outcome: investigation.RunSucceeded,
		WindowFrom: now.Add(-time.Hour), WindowUntil: now, StartedAt: now, FinishedAt: now}
	if err := database.RecordToolRun(ctx, org, id, claimToken(t, database, org, id), run); err == nil {
		t.Fatal("expired worker persisted a Tool Run")
	}
}

func TestExpiredWorkerCannotAppendProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	id := aTurn(t, database, org)
	if _, _, claimed, err := database.ClaimInvestigation(ctx, aClaim("worker")); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	expireInvestigationLease(t, database, org, id)
	if err := database.AppendEvent(ctx, org, id, claimToken(t, database, org, id), investigation.Event{Sequence: 1, At: time.Now(), Type: investigation.EventProgress}); err == nil {
		t.Fatal("expired worker appended progress")
	}
}

func TestExpiredLeaseCannotBeRenewedBeforeRecovery(t *testing.T) {
	t.Parallel()
	database, organization := migratedDatabase(t)
	ctx := context.Background()
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := openTurnForTest(database, ctx, organization, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	claim := aClaim("original-worker")
	if _, _, took, err = database.ClaimInvestigation(ctx, claim); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	expireInvestigationLease(t, database, organization, turn.InvestigationID)
	claim.Token = claimToken(t, database, organization, turn.InvestigationID)
	if held, err := database.Heartbeat(ctx, organization, turn.InvestigationID, claim); err != nil || held {
		t.Fatalf("expired lease renewed: held=%v err=%v", held, err)
	}
	if recovered, err := database.RecoverStale(ctx, investigation.RecoveryReason, 10); err != nil || recovered != 1 {
		t.Fatalf("recovering expired lease: recovered=%d err=%v", recovered, err)
	}
	found, err := database.Investigation(ctx, organization, turn.InvestigationID)
	if err != nil || found.Status != investigation.StatusFailed {
		t.Fatalf("recovered status=%v err=%v", found.Status, err)
	}
}

func aClaim(worker string) investigation.Claim {
	return investigation.Claim{
		Worker: worker, LeaseFor: turnWindowLead,
	}
}

func expireInvestigationLease(
	t *testing.T, database *storage.Database, organization uuid.UUID,
	id uuid.UUID,
) {
	t.Helper()

	pool, err := poolForTest(database, organization)
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

func TestEveryInvestigationIsClaimedExactlyOnce(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)

	const turns = 8
	opened := make(map[uuid.UUID]bool, turns)
	for range turns {
		conversation := openConversation(t, database, organization, "checkout is slow")
		say(t, database, organization, conversation.ID, "what changed?")
		turn, took, err := openTurnForTest(database, context.Background(), organization,
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
		turn, took, err := openTurnForTest(database, context.Background(), organization,
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
	turn, took, err := openTurnForTest(database, context.Background(), organization,
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
		turn.InvestigationID, investigation.Claim{Worker: "worker-a", LeaseFor: turnWindowLead, Token: claimToken(t, database, organization, turn.InvestigationID)}); heartbeatErr != nil || !held {
		t.Fatalf("the holder could not renew its lease: held=%v err=%v", held, heartbeatErr)
	}
}

func TestALapsedLeaseFailsTheInvestigationAndEndsItsStream(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := openTurnForTest(database, context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}

	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-that-dies")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}
	if err = database.AppendEvent(context.Background(), organization,
		turn.InvestigationID, claimToken(t, database, organization, turn.InvestigationID), investigation.Event{
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

func TestARecoveredInvestigationIsNotClaimedAgain(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := openTurnForTest(database, context.Background(), organization,
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

func TestConcludingReleasesTheLease(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	conversation := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, conversation.ID, "what changed?")
	turn, took, err := openTurnForTest(database, context.Background(), organization,
		conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	if _, _, took, err = database.ClaimInvestigation(context.Background(),
		aClaim("worker-a")); err != nil || !took {
		t.Fatalf("claiming: took=%v err=%v", took, err)
	}

	if err = database.ConcludeInvestigation(context.Background(), organization,
		turn.InvestigationID, claimToken(t, database, organization, turn.InvestigationID), conclusionSaying("done"), "",
		investigation.Usage{}); err != nil {
		t.Fatalf("concluding: %v", err)
	}

	if held, heartbeatErr := database.Heartbeat(context.Background(), organization,
		turn.InvestigationID, aClaim("worker-a")); heartbeatErr != nil || held {
		t.Errorf("a concluded investigation still holds a lease: held=%v err=%v",
			held, heartbeatErr)
	}
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
				claimToken(t, database, organization, id), conclusionSaying("done"), "", usage)
		}},
		{name: "failed", end: func(id uuid.UUID, usage investigation.Usage) error {
			return database.FailInvestigation(context.Background(), organization, id,
				claimToken(t, database, organization, id), "provider unavailable", usage)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversation := openConversation(t, database, organization, test.name)
			say(t, database, organization, conversation.ID, "what happened?")
			turn, took, err := openTurnForTest(database, context.Background(), organization,
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

func TestFailedEventWritePreservesStreamStateForRetry(t *testing.T) {
	for _, payload := range []investigation.EventPayload{investigation.ProgressPayload("reading"), investigation.FailedPayload("failed")} {
		eventType := payload.EventType()
		t.Run(eventType.String(), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			id := aTurn(t, database, org)
			pool, err := poolForTest(database, org)
			if err != nil {
				t.Fatal(err)
			}
			token := claimToken(t, database, org, id)
			stream := investigation.NewEventStream(func(ctx context.Context, org uuid.UUID, id uuid.UUID, event investigation.Event) error {
				return database.AppendEvent(ctx, org, id, token, event)
			}, nil, org, id)
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event ADD CONSTRAINT reject_stream_test CHECK (type NOT IN (2, 7))`); err != nil {
				t.Fatal(err)
			}
			if err = stream.Emit(ctx, payload); err == nil {
				t.Fatal("event write failure was swallowed")
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event DROP CONSTRAINT reject_stream_test`); err != nil {
				t.Fatal(err)
			}
			if err = stream.Emit(ctx, payload); err != nil {
				t.Fatal(err)
			}
			events, err := database.Events(ctx, org, id, 0, 0)
			if err != nil || len(events) != 1 || events[0].Sequence != 1 || events[0].Type != eventType {
				t.Fatalf("failed write consumed stream state: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestCancellationRacesCompletionAndStaleOwnership(t *testing.T) {
	database, org := migratedDatabase(t)
	ctx := context.Background()
	for range 10 {
		id := aTurn(t, database, org)
		token := claimToken(t, database, org, id)
		start := make(chan struct{})
		finished := make(chan error, 2)
		stale := make(chan error, 1)
		go func() {
			<-start
			finished <- database.ConcludeInvestigation(ctx, org, id, token,
				conclusionSaying("committed answer"), "", investigation.Usage{})
		}()
		principal := ownerOf(t, org)
		go func() {
			<-start
			_, err := database.CancelInvestigation(ctx, principal, org, id)
			finished <- err
		}()
		go func() {
			<-start
			stale <- database.FailInvestigation(ctx, org, id, uuid.New(), "stale failure", investigation.Usage{})
		}()
		close(start)
		winners := 0
		for range 2 {
			err := <-finished
			if err == nil {
				winners++
			} else if !errors.Is(err, investigation.ErrUnknown) && !errors.Is(err, investigation.ErrAlreadyEnded) {
				t.Fatal(err)
			}
		}
		if err := <-stale; !errors.Is(err, investigation.ErrUnknown) {
			t.Fatalf("stale writer result = %v", err)
		}
		if winners != 1 {
			t.Fatalf("terminal winners=%d, want one", winners)
		}
		events, err := database.Events(ctx, org, id, 0, 0)
		if err != nil || len(events) != 1 {
			t.Fatalf("events=%+v, error=%v", events, err)
		}
		result, err := database.Investigation(ctx, org, id)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == investigation.StatusConcluded {
			if events[0].Type != investigation.EventConcluded || events[0].Payload["summary"] != result.Conclusion.Summary {
				t.Fatal("conclusion and replay disagree")
			}
		} else if result.Status != investigation.StatusCancelled || events[0].Type != investigation.EventCancelled {
			t.Fatal("cancellation and replay disagree")
		}
	}
}

func TestConclusionCommitsItsReplayEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	conversation := openConversation(t, database, org, "durable completion")
	say(t, database, org, conversation.ID, "what happened?")
	turn, took, err := openTurnForTest(database, ctx, org, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID,
		claimToken(t, database, org, turn.InvestigationID), conclusionSaying("canonical answer"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	events, err := database.Events(ctx, org, turn.InvestigationID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != investigation.EventConcluded ||
		events[0].Payload["summary"] != "canonical answer" {
		t.Fatalf("completion returned without its replay event: %+v", events)
	}
}

func TestTerminalEventFailureRollsBackOutcome(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "concluded", true: "failed"}[failure], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			conversation := openConversation(t, database, org, "terminal rollback")
			say(t, database, org, conversation.ID, "what happened?")
			turn, took, err := openTurnForTest(database, ctx, org, conversation.ID, turnWindowLead)
			if err != nil || !took {
				t.Fatalf("opening turn: took=%v err=%v", took, err)
			}
			pool, err := poolForTest(database, org)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event ADD CONSTRAINT reject_terminal_test CHECK (type NOT IN (6, 7))`); err != nil {
				t.Fatal(err)
			}
			finish := func() error {
				if failure {
					return database.FailInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), "provider unavailable", investigation.Usage{InputTokens: 42})
				}
				return database.ConcludeInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), conclusionSaying("answer"), "", investigation.Usage{InputTokens: 42})
			}
			if err = finish(); err == nil {
				t.Fatal("terminal event failure was swallowed")
			}
			found, err := database.Investigation(ctx, org, turn.InvestigationID)
			if err != nil {
				t.Fatal(err)
			}
			if found.Status != investigation.StatusRunning || found.Usage.InputTokens != 0 || found.Conclusion.Summary != "" || !found.ConcludedAt.IsZero() {
				t.Fatalf("terminal failure committed outcome: %+v", found)
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event DROP CONSTRAINT reject_terminal_test`); err != nil {
				t.Fatal(err)
			}
			if err = finish(); err != nil {
				t.Fatal(err)
			}
			events, err := database.Events(ctx, org, turn.InvestigationID, 0, 0)
			if err != nil || len(events) != 1 || events[0].Sequence != 1 || !events[0].Type.Terminal() {
				t.Fatalf("retry lost terminal event or sequence: %+v err=%v", events, err)
			}
		})
	}
}

func TestLateProgressCannotFollowCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, org := migratedDatabase(t)
	conversation := openConversation(t, database, org, "no late progress")
	say(t, database, org, conversation.ID, "what happened?")
	turn, took, err := openTurnForTest(database, ctx, org, conversation.ID, turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening turn: took=%v err=%v", took, err)
	}
	if err = database.ConcludeInvestigation(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), conclusionSaying("done"), "", investigation.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err = database.AppendEvent(ctx, org, turn.InvestigationID, claimToken(t, database, org, turn.InvestigationID), investigation.Event{Sequence: 2, Type: investigation.EventProgress}); err == nil {
		t.Fatal("completed Investigation accepted late activity")
	}
}

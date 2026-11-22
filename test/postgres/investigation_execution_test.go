package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func claimFixtureInvestigation(ctx context.Context, db *storage.Database) (uuid.UUID, investigation.Investigation, bool, error) {
	org, claim, err := db.ClaimNextAgentExecution(ctx, time.Minute)
	if errors.Is(err, storage.ErrAgentExecutionUnavailable) {
		return uuid.Nil, investigation.Investigation{}, false, nil
	}
	if err != nil {
		return uuid.Nil, investigation.Investigation{}, false, err
	}
	opened, err := db.Investigation(ctx, org, claim.InvestigationID)
	return org, opened, err == nil, err
}

func seedInvestigationEvent(ctx context.Context, db *storage.Database, org, id uuid.UUID, event investigation.Event) error {
	pool, err := poolForTest(db, org)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
		SELECT $1, $2, coalesce(max(sequence), 0) + 1, $3, $4
		FROM investigation_event WHERE org_id = $1 AND investigation_id = $2`, org, id, int16(event.Type), payload)
	return err
}

// seedInvestigationConclusion prepares retained records for Conversation and delivery reads.
func seedInvestigationConclusion(ctx context.Context, db *storage.Database, org, id uuid.UUID, conclusion investigation.Conclusion) error {
	pool, err := poolForTest(db, org)
	if err != nil {
		return err
	}
	report, err := json.Marshal(conclusion)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `WITH ended AS (
		UPDATE investigation SET status = 2, conclusion = $3, concluded_at = now()
		 WHERE org_id = $1 AND investigation_id = $2 RETURNING *
	), finished AS (
		UPDATE agent_execution SET status = 'completed', lease_expires_at = NULL, lease_token = NULL
		 WHERE org_id = $1 AND investigation_id = $2
	), reply AS (
		INSERT INTO conversation_message (org_id, conversation_id, sequence, role, text, investigation_id, window_from, window_until)
		SELECT i.org_id, i.conversation_id, (SELECT coalesce(max(m.sequence), 0) + 1 FROM conversation_message m
			WHERE m.org_id = i.org_id AND m.conversation_id = i.conversation_id), 2, $4, i.investigation_id, i.window_from, i.window_until
		FROM ended i WHERE i.conversation_id IS NOT NULL
	)
	INSERT INTO investigation_event (org_id, investigation_id, sequence, type, payload)
	SELECT i.org_id, i.investigation_id, (SELECT coalesce(max(sequence), 0) + 1 FROM investigation_event
		WHERE org_id = i.org_id AND investigation_id = i.investigation_id), $5,
		jsonb_build_object('summary', $4::text, 'status', $6::text) FROM ended i`,
		org, id, report, conclusion.Summary, int16(investigation.EventConcluded), string(conclusion.Status))
	return err
}

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
		if err := seedInvestigationEvent(context.Background(), database, organization, id, investigation.Event{
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
	if err = seedInvestigationEvent(ctx, database, organization, id, investigation.Event{
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
	if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event DROP CONSTRAINT investigation_event_type_check`); err != nil {
		t.Fatal(err)
	}
	for sequence, eventType := range []int16{1, 5, 2, 8, 10, 6} {
		if _, err = pool.Exec(ctx, `
			INSERT INTO investigation_event
				(org_id, investigation_id, sequence, type, payload, at)
			VALUES ($1, $2, $3, $4, '{}', now())`,
			organization, id, sequence+1, eventType); err != nil {
			t.Fatalf("seeding event %d: %v", sequence+1, err)
		}
	}
	if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event ADD CONSTRAINT investigation_event_type_check
		CHECK (type IN (1, 2, 3, 4, 6, 7, 9)) NOT VALID`); err != nil {
		t.Fatal(err)
	}

	events, err := database.Events(ctx, organization, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("replay returned %d active events, want 3: %+v", len(events), events)
	}
	wantSequences := []int64{1, 3, 6}
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
		"tool":      "slack_get_channel_history",
		"ordinal":   float64(3),
		"truncated": true,
		"arguments": map[string]any{"channel": "deploys"},
	}
	if err := seedInvestigationEvent(context.Background(), database, organization, id, investigation.Event{
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
	if read[0].Payload["tool"] != "slack_get_channel_history" ||
		read[0].Payload["ordinal"] != float64(3) ||
		read[0].Payload["truncated"] != true {
		t.Errorf("payload = %+v, want %+v", read[0].Payload, payload)
	}
	arguments, ok := read[0].Payload["arguments"].(map[string]any)
	if !ok || arguments["channel"] != "deploys" {
		t.Errorf("nested arguments did not survive: %+v", read[0].Payload["arguments"])
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

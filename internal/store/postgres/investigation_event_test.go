package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// The event stream where it has to be durable. Replay is the whole reason these are rows
// rather than a broadcast: a reader that reconnects and one that landed on another replica
// both ask for what comes after a sequence, and both are answered from here.

// aTurn opens a conversation, asks something, and returns the investigation the turn
// created — an event stream needs an investigation to hang off.
func aTurn(
	t *testing.T, database *storage.Database, organization tenancy.Organization,
) uuid.UUID {
	t.Helper()

	opened := openConversation(t, database, organization, "checkout is slow")
	say(t, database, organization, opened.ID, "what changed?")
	turn, took, err := database.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	return turn.InvestigationID
}

// appendEvents writes a scripted run's worth of events.
func appendEvents(
	t *testing.T, database *storage.Database, organization tenancy.Organization,
	id uuid.UUID, types ...investigation.EventType,
) {
	t.Helper()

	for position, eventType := range types {
		if err := database.AppendEvent(context.Background(), organization, id,
			investigation.Event{
				Sequence: int64(position + 1),
				At:       time.Now().UTC(),
				Type:     eventType,
				Payload:  map[string]any{"position": position},
			}); err != nil {
			t.Fatalf("appending event %d: %v", position+1, err)
		}
	}
}

// Resuming from an arbitrary point produces exactly the missing suffix — no gap and no
// repeat. This is the entire reconnect contract.
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
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	// Model a database already migrated by a newer binary. The current schema correctly
	// refuses values it cannot write; a rolling-back reader must still preserve their row
	// and continue reading the known history around it.
	if _, err = pool.Exec(ctx,
		`ALTER TABLE investigation_event DROP CONSTRAINT investigation_event_type_check`); err != nil {
		t.Fatalf("modeling a newer event schema: %v", err)
	}
	if _, err = pool.Exec(ctx, `
		INSERT INTO investigation_event
			(org_id, investigation_id, sequence, type, payload, at)
		VALUES ($1, $2, 1, 32000, '{"future":"preserved"}', now())`,
		organization.String(), id); err != nil {
		t.Fatalf("seeding future history: %v", err)
	}
	if err = database.AppendEvent(ctx, organization, id, investigation.Event{
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
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	for sequence, eventType := range []int16{1, 5, 2, 8, 6} {
		if _, err = pool.Exec(ctx, `
			INSERT INTO investigation_event
				(org_id, investigation_id, sequence, type, payload, at)
			VALUES ($1, $2, $3, $4, '{}', now())`,
			organization.String(), id, sequence+1, eventType); err != nil {
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

// A payload round-trips through JSONB unchanged, so what a reader is handed is what the
// platform composed.
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
		investigation.Event{
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

// TWO WRITERS AT ONE POSITION. The lease makes one writer per investigation; the primary
// key is the backstop for the case where that turns out to be false. A second row at a
// sequence that already exists is REFUSED rather than silently overwriting the first,
// because a stream that quietly changed what it already said is worse than one that stops.
func TestASecondWriterAtTheSameSequenceIsRefused(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)

	first := investigation.Event{
		Sequence: 1, At: time.Now().UTC(), Type: investigation.EventStarted,
		Payload: map[string]any{"writer": "the lease holder"},
	}
	if err := database.AppendEvent(
		context.Background(), organization, id, first); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}

	second := first
	second.Payload = map[string]any{"writer": "somebody who should not be here"}
	if err := database.AppendEvent(
		context.Background(), organization, id, second); err == nil {
		t.Fatal("a second event at sequence 1 was accepted; the primary key is the " +
			"backstop against a double-claim and it must refuse")
	}

	read, err := database.Events(context.Background(), organization, id, 0, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(read) != 1 || read[0].Payload["writer"] != "the lease holder" {
		t.Errorf("the refused write changed the stream: %+v", read)
	}
}

// The events go with the investigation. Cascade delete is what keeps the investigation the
// single retention unit, so no second reaper has to exist.
func TestEventsAreDeletedWithTheirInvestigation(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	id := aTurn(t, database, organization)
	appendEvents(t, database, organization, id, investigation.EventStarted,
		investigation.EventConcluded)

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if _, err = pool.Exec(context.Background(), `
		DELETE FROM conversation_message WHERE investigation_id = $1`, id); err != nil {
		t.Fatalf("detaching the turn's messages: %v", err)
	}
	if _, err = pool.Exec(context.Background(), `
		DELETE FROM investigation WHERE investigation_id = $1`, id); err != nil {
		t.Fatalf("deleting the investigation: %v", err)
	}

	read, err := database.Events(context.Background(), organization, id, 0, 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(read) != 0 {
		t.Errorf("%d events survived their investigation; the cascade is what keeps the "+
			"investigation the single retention unit", len(read))
	}
}

// An investigation identifier from another tenant reads as an empty stream, never as
// somebody else's events.
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

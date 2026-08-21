package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The event stream where it has to be durable. Replay is the whole reason these are rows
// rather than a broadcast: a reader that reconnects and one that landed on another replica
// both ask for what comes after a sequence, and both are answered from here.

// aTurn opens a conversation, asks something, and returns the investigation the turn
// created — an event stream needs an investigation to hang off.
func aTurn(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
) uuid.UUID {
	t.Helper()

	opened := openConversation(t, placements, organization, "checkout is slow")
	say(t, placements, organization, opened.ID, "what changed?")
	turn, took, err := placements.OpenTurn(context.Background(), organization, opened.ID,
		turnWindowLead)
	if err != nil || !took {
		t.Fatalf("opening a turn: took=%v err=%v", took, err)
	}
	return turn.InvestigationID
}

// appendEvents writes a scripted run's worth of events.
func appendEvents(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	id uuid.UUID, types ...investigation.EventType,
) {
	t.Helper()

	for position, eventType := range types {
		if err := placements.AppendEvent(context.Background(), organization, id,
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

	placements, organization := migratedPlacement(t)
	id := aTurn(t, placements, organization)

	written := []investigation.EventType{
		investigation.EventStarted,
		investigation.EventToolStarted,
		investigation.EventToolCompleted,
		investigation.EventProgress,
		investigation.EventAnswerDelta,
		investigation.EventConcluded,
	}
	appendEvents(t, placements, organization, id, written...)

	for after := range len(written) + 1 {
		read, err := placements.Events(context.Background(), organization, id,
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

// A payload round-trips through JSONB unchanged, so what a reader is handed is what the
// platform composed.
func TestAnEventPayloadSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	id := aTurn(t, placements, organization)

	payload := map[string]any{
		"tool":      "slack.get_channel_history",
		"ordinal":   float64(3),
		"truncated": true,
		"arguments": map[string]any{"channel": "deploys"},
	}
	if err := placements.AppendEvent(context.Background(), organization, id,
		investigation.Event{
			Sequence: 1, At: time.Now().UTC(),
			Type: investigation.EventToolStarted, Payload: payload,
		}); err != nil {
		t.Fatalf("appending: %v", err)
	}

	read, err := placements.Events(context.Background(), organization, id, 0, 0)
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

	placements, organization := migratedPlacement(t)
	id := aTurn(t, placements, organization)

	first := investigation.Event{
		Sequence: 1, At: time.Now().UTC(), Type: investigation.EventStarted,
		Payload: map[string]any{"writer": "the lease holder"},
	}
	if err := placements.AppendEvent(
		context.Background(), organization, id, first); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}

	second := first
	second.Payload = map[string]any{"writer": "somebody who should not be here"}
	if err := placements.AppendEvent(
		context.Background(), organization, id, second); err == nil {
		t.Fatal("a second event at sequence 1 was accepted; the primary key is the " +
			"backstop against a double-claim and it must refuse")
	}

	read, err := placements.Events(context.Background(), organization, id, 0, 0)
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

	placements, organization := migratedPlacement(t)
	id := aTurn(t, placements, organization)
	appendEvents(t, placements, organization, id, investigation.EventStarted,
		investigation.EventConcluded)

	pool, err := placements.Pool(organization)
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

	read, err := placements.Events(context.Background(), organization, id, 0, 0)
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

	placements, mine, theirs := twoOrganizationsOnOnePlacement(t)
	id := aTurn(t, placements, mine)
	appendEvents(t, placements, mine, id, investigation.EventStarted,
		investigation.EventConcluded)

	read, err := placements.Events(context.Background(), theirs, id, 0, 0)
	if err != nil {
		t.Fatalf("reading across tenants: %v", err)
	}
	if len(read) != 0 {
		t.Errorf("%d events readable from another organization", len(read))
	}
}

package storage_test

import (
	"context"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestFailedEventWritePreservesStreamStateForRetry(t *testing.T) {
	for _, eventType := range []investigation.EventType{investigation.EventProgress, investigation.EventFailed} {
		t.Run(eventType.String(), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database, org := migratedDatabase(t)
			id := aTurn(t, database, org)
			pool, err := database.Pool(org)
			if err != nil {
				t.Fatal(err)
			}
			stream := investigation.NewEventStream(database.AppendEvent, nil, org, id)
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event ADD CONSTRAINT reject_stream_test CHECK (type NOT IN (2, 7))`); err != nil {
				t.Fatal(err)
			}
			if err = stream.Emit(ctx, eventType, nil); err == nil {
				t.Fatal("event write failure was swallowed")
			}
			if _, err = pool.Exec(ctx, `ALTER TABLE investigation_event DROP CONSTRAINT reject_stream_test`); err != nil {
				t.Fatal(err)
			}
			if err = stream.Emit(ctx, eventType, nil); err != nil {
				t.Fatal(err)
			}
			events, err := database.Events(ctx, org, id, 0, 0)
			if err != nil || len(events) != 1 || events[0].Sequence != 1 || events[0].Type != eventType {
				t.Fatalf("failed write consumed stream state: events=%+v err=%v", events, err)
			}
		})
	}
}

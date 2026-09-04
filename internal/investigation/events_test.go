package investigation

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEveryDocumentedEventHasItsExactSSEWireName(t *testing.T) {
	t.Parallel()
	documented := []struct {
		typeID EventType
		name   string
	}{
		{EventStarted, "started"},
		{EventProgress, "progress"},
		{EventToolStarted, "tool_started"},
		{EventToolCompleted, "tool_completed"},
		{EventHypothesesUpdated, "hypotheses_updated"},
		{EventConcluded, "concluded"},
		{EventFailed, "failed"},
		{EventCancelled, "cancelled"},
	}
	for index, event := range documented {
		recorder := httptest.NewRecorder()
		if err := writeEvent(recorder, eventEnvelope{
			Sequence: int64(index + 1), Type: event.typeID.String(),
		}); err != nil {
			t.Fatalf("writing %s: %v", event.name, err)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "event: "+event.name+"\n") ||
			!strings.Contains(body, `"type":"`+event.name+`"`) {
			t.Errorf("%s serialized with the wrong wire name:\n%s", event.name, body)
		}
	}
	for _, retired := range []EventType{5, 8} {
		if retired.String() != "unrecognised" {
			t.Errorf("retired event type %d is still active as %q", retired, retired.String())
		}
	}
}

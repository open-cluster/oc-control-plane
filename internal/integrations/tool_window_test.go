package integrations

import (
	"testing"
	"time"
)

// CLAMPING NEVER INVERTS.
//
// The clamp intersects the model's ask with the investigation's window. An ask that falls
// entirely outside the window has an EMPTY intersection, and the arithmetic that produces
// it can put the start after the end — a window that reads as a nonsense instruction to
// whatever consumes it, and which is now shown to the model and written to the run record.
// An empty window is expressed as a zero-width one, never as a backwards one.

func window(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}

func TestClampingNarrowsIntoTheInvestigationWindow(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	from, until := request.ClampWindow(
		window(t, "2026-01-01T00:00:00Z"), window(t, "2026-12-31T00:00:00Z"))

	if !from.Equal(request.WindowFrom) || !until.Equal(request.WindowUntil) {
		t.Errorf("clamped to %v..%v; a wider ask must not widen the read", from, until)
	}
}

func TestAnAskEntirelyAfterTheWindowDoesNotInvertIt(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	// The model asks about a month that starts after the investigation's window ends.
	from, until := request.ClampWindow(window(t, "2026-09-01T00:00:00Z"), time.Time{})

	if from.After(until) {
		t.Errorf("clamped to %v..%v, which starts after it ends", from, until)
	}
}

func TestAnAskEntirelyBeforeTheWindowDoesNotInvertIt(t *testing.T) {
	t.Parallel()

	request := ToolRequest{
		WindowFrom:  window(t, "2026-08-22T08:00:00Z"),
		WindowUntil: window(t, "2026-08-22T10:00:00Z"),
	}
	from, until := request.ClampWindow(time.Time{}, window(t, "2026-07-01T00:00:00Z"))

	if from.After(until) {
		t.Errorf("clamped to %v..%v, which starts after it ends", from, until)
	}
}

package integrations

import (
	"testing"
	"time"
)

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

package integrations

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The shared argument reader: both providers used to carry a private copy of this, and
// the copies were where a security invariant lived. These tests pin the one shared
// behavior the providers now call.

func declaredArguments() []ToolArgument {
	return []ToolArgument{
		{Name: "channel", Description: "the channel id", Type: FieldString, Required: true},
		{Name: "nameContains", Description: "a filter", Type: FieldString},
		{Name: "repositoryId", Description: "the stable id", Type: FieldInteger, Required: true},
		{Name: "limit", Description: "how many", Type: FieldInteger},
		{Name: "since", Description: "start of the window", Type: FieldString},
	}
}

func TestReadArgumentsRefusesAnUndeclaredArgument(t *testing.T) {
	_, err := ReadArguments(declaredArguments(), map[string]any{"unheard": "of"})
	if err == nil || !strings.Contains(err.Error(), "unheard") {
		t.Fatalf("an undeclared argument must be refused by name, got %v", err)
	}
}

func TestTextReadsAnOptionalString(t *testing.T) {
	values, err := ReadArguments(declaredArguments(), map[string]any{"nameContains": "  incident  "})
	if err != nil {
		t.Fatal(err)
	}
	text, err := values.Text("nameContains")
	if err != nil || text != "incident" {
		t.Fatalf("Text = %q, %v; want trimmed \"incident\"", text, err)
	}
	absent, err := values.Text("since")
	if err != nil || absent != "" {
		t.Fatalf("an absent optional string must read empty, got %q, %v", absent, err)
	}
	if _, err := readAs(values, "nameContains", 7); err == nil {
		t.Fatal("a non-string value must be refused")
	}
}

// readAs re-reads with the named value replaced, so type refusals stay one-liners.
func readAs(values Arguments, name string, value any) (string, error) {
	replaced, err := ReadArguments(declaredArguments(), map[string]any{name: value})
	if err != nil {
		return "", err
	}
	return replaced.Text(name)
}

func TestRequiredRefusesAbsenceAndBlank(t *testing.T) {
	values, err := ReadArguments(declaredArguments(), map[string]any{"channel": "  "})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := values.Required("channel"); err == nil {
		t.Fatal("a blank required string must be refused")
	}
	empty, err := ReadArguments(declaredArguments(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Required("channel"); err == nil {
		t.Fatal("an absent required string must be refused")
	}
	given, _ := ReadArguments(declaredArguments(), map[string]any{"channel": "C123"})
	channel, err := given.Required("channel")
	if err != nil || channel != "C123" {
		t.Fatalf("Required = %q, %v", channel, err)
	}
}

func TestIdentityReadsAWholePositiveNumber(t *testing.T) {
	values, _ := ReadArguments(declaredArguments(), map[string]any{"repositoryId": float64(101)})
	id, err := values.Identity("repositoryId")
	if err != nil || id != 101 {
		t.Fatalf("Identity = %d, %v", id, err)
	}
	for _, wrong := range []any{float64(0), float64(-3), float64(1.5), "101"} {
		broken, _ := ReadArguments(declaredArguments(), map[string]any{"repositoryId": wrong})
		if _, err := broken.Identity("repositoryId"); err == nil {
			t.Fatalf("%v must be refused as an identity", wrong)
		}
	}
	absent, _ := ReadArguments(declaredArguments(), map[string]any{})
	if _, err := absent.Identity("repositoryId"); err == nil {
		t.Fatal("an absent identity must be refused, never defaulted")
	}
}

func TestCountDefaultsAndBounds(t *testing.T) {
	absent, _ := ReadArguments(declaredArguments(), map[string]any{})
	count, err := absent.Count("limit", 30, 100)
	if err != nil || count != 30 {
		t.Fatalf("an absent count must fall back to the default, got %d, %v", count, err)
	}
	given, _ := ReadArguments(declaredArguments(), map[string]any{"limit": float64(70)})
	if count, err = given.Count("limit", 30, 100); err != nil || count != 70 {
		t.Fatalf("Count = %d, %v", count, err)
	}
	for _, wrong := range []any{float64(0), float64(101), float64(2.5), "12"} {
		broken, _ := ReadArguments(declaredArguments(), map[string]any{"limit": wrong})
		if _, err := broken.Count("limit", 30, 100); err == nil {
			t.Fatalf("%v must be refused as a count", wrong)
		}
	}
}

func TestMomentReadsRFC3339(t *testing.T) {
	values, _ := ReadArguments(declaredArguments(),
		map[string]any{"since": "2026-01-02T15:04:05Z"})
	at, err := values.Moment("since")
	if err != nil || !at.Equal(time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Fatalf("Moment = %v, %v", at, err)
	}
	absent, _ := ReadArguments(declaredArguments(), map[string]any{})
	if at, err := absent.Moment("since"); err != nil || !at.IsZero() {
		t.Fatalf("an absent moment must read zero, got %v, %v", at, err)
	}
	broken, _ := ReadArguments(declaredArguments(), map[string]any{"since": "yesterday"})
	if _, err := broken.Moment("since"); err == nil {
		t.Fatal("a non-RFC-3339 moment must be refused")
	}
}

func TestClampWindowNarrowsIntoTheRequests(t *testing.T) {
	from := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	request := ToolRequest{WindowFrom: from, WindowUntil: until}

	// A wider ask does not widen the read.
	since, latest := request.ClampWindow(from.Add(-time.Hour), until.Add(time.Hour))
	if !since.Equal(from) || !latest.Equal(until) {
		t.Fatalf("a wider ask must clamp to the investigation's window, got %v..%v", since, latest)
	}
	// A narrower ask stands.
	narrowFrom, narrowUntil := from.Add(30*time.Minute), until.Add(-30*time.Minute)
	since, latest = request.ClampWindow(narrowFrom, narrowUntil)
	if !since.Equal(narrowFrom) || !latest.Equal(narrowUntil) {
		t.Fatalf("a narrower ask must stand, got %v..%v", since, latest)
	}
	// Zero arguments take the whole window.
	since, latest = request.ClampWindow(time.Time{}, time.Time{})
	if !since.Equal(from) || !latest.Equal(until) {
		t.Fatalf("zero arguments must take the window, got %v..%v", since, latest)
	}
	// A direct call outside any investigation clamps nothing.
	direct := ToolRequest{}
	since, latest = direct.ClampWindow(narrowFrom, narrowUntil)
	if !since.Equal(narrowFrom) || !latest.Equal(narrowUntil) {
		t.Fatalf("an unbounded request must leave the ask alone, got %v..%v", since, latest)
	}
}

func TestReadArgumentsErrorsAreForOperators(t *testing.T) {
	// The refusals travel into recorded run errors, so they must be sentences, not codes.
	broken, _ := ReadArguments(declaredArguments(), map[string]any{"limit": "many"})
	_, err := broken.Count("limit", 30, 100)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a refusal must name the argument, got %v", err)
	}
	var target *time.ParseError
	if errors.As(err, &target) {
		t.Fatal("a refusal must not leak a library's own error type")
	}
}

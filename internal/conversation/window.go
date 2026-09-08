package conversation

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidWindow  = errors.New("windowFrom and windowUntil must be paired RFC3339 timestamps with an offset, windowFrom before windowUntil, and windowUntil not in the future")
	ErrWindowConflict = errors.New("the queued batch has a different window; retry after it opens")
)

// Window is a half-open interval, normalized to PostgreSQL timestamp precision.
type Window struct {
	From  time.Time
	Until time.Time
}

// Normalized converts the window to UTC and PostgreSQL timestamp precision.
func (w Window) Normalized() Window {
	return Window{
		From:  w.From.UTC().Truncate(time.Microsecond),
		Until: w.Until.UTC().Truncate(time.Microsecond),
	}
}

func (w Window) Valid(now time.Time) bool {
	return w.From.Year() >= 1 && w.From.Before(w.Until) && !w.Until.After(now)
}

type windowInput struct {
	WindowFrom  json.RawMessage `json:"windowFrom"`
	WindowUntil json.RawMessage `json:"windowUntil"`
}

func (input windowInput) parse(now time.Time) (*Window, error) {
	if len(input.WindowFrom) == 0 && len(input.WindowUntil) == 0 {
		return nil, nil
	}

	var from, until string
	if json.Unmarshal(input.WindowFrom, &from) != nil || json.Unmarshal(input.WindowUntil, &until) != nil {
		return nil, ErrInvalidWindow
	}
	parsedFrom, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return nil, ErrInvalidWindow
	}
	parsedUntil, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return nil, ErrInvalidWindow
	}
	w := Window{
		From:  parsedFrom,
		Until: parsedUntil,
	}.Normalized()

	if !w.Valid(now) {
		return nil, ErrInvalidWindow
	}
	return &w, nil
}

// MinimumQuestionWindow is the default lookback used for questions without an incident.
const MinimumQuestionWindow = 24 * time.Hour

const DefaultIncidentWindowLead = 2 * time.Hour

// QuestionWindow preserves the default lookback floor when no explicit window is supplied.
func QuestionWindow(lead time.Duration) time.Duration {
	if lead > MinimumQuestionWindow {
		return lead
	}
	return MinimumQuestionWindow
}

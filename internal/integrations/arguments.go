package integrations

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The shared argument reader. Every provider's tools read their calls through this one
// implementation, so the refusal semantics — undeclared arguments refused rather than
// dropped, whole numbers required rather than truncated — cannot drift copy by copy. The
// third provider gets these lines for free instead of copying the ones a security
// invariant lives in.

// Arguments is one call's inputs after the undeclared ones were refused.
type Arguments struct {
	values map[string]any
}

// ReadArguments refuses an argument nothing declares. Dropped arguments are the quiet
// failure mode of tool calling: the caller believes it narrowed the read and it did not.
func ReadArguments(declared []ToolArgument, given map[string]any) (Arguments, error) {
	for name := range given {
		if !declaresArgument(declared, name) {
			return Arguments{}, fmt.Errorf("argument %q is not one this tool declares", name)
		}
	}
	return Arguments{values: given}, nil
}

func declaresArgument(declared []ToolArgument, name string) bool {
	for _, argument := range declared {
		if argument.Name == name {
			return true
		}
	}
	return false
}

// Text reads an optional string argument, trimmed.
func (a Arguments) Text(name string) (string, error) {
	value, given := a.values[name]
	if !given {
		return "", nil
	}
	text, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("%s must be text", name)
	}
	return strings.TrimSpace(text), nil
}

// Required reads a string argument that must be present and non-empty.
func (a Arguments) Required(name string) (string, error) {
	text, err := a.Text(name)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New(name + " is required")
	}
	return text, nil
}

// Identity reads a required positive whole number, the shape every stable id has.
// Decoded JSON numbers arrive as float64; a whole positive value is required, not
// merely truncated.
func (a Arguments) Identity(name string) (int64, error) {
	value, given := a.values[name]
	if !given {
		return 0, errors.New(name + " is required")
	}
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 {
		return 0, fmt.Errorf("%s must be a whole positive number", name)
	}
	return int64(number), nil
}

// Count reads a bounded whole number, applying the default when absent.
func (a Arguments) Count(name string, fallback, maximum int) (int, error) {
	value, given := a.values[name]
	if !given {
		return fallback, nil
	}
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 || int(number) > maximum {
		return 0, fmt.Errorf("%s must be a whole number between 1 and %d", name, maximum)
	}
	return int(number), nil
}

// Moment reads an optional RFC 3339 timestamp.
func (a Arguments) Moment(name string) (time.Time, error) {
	text, err := a.Text(name)
	if err != nil || text == "" {
		return time.Time{}, err
	}
	at, parseErr := time.Parse(time.RFC3339, text)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 time such as "+
			"2026-01-02T15:04:05Z", name)
	}
	return at, nil
}

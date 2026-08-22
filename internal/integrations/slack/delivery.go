package slack

import (
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// WHAT AN INVESTIGATION LOOKS LIKE IN A SLACK THREAD.
//
// The mapping is here, in the provider package, and the investigation knows nothing about it.
// This file takes the persisted event stream — the same stream the console renders — and turns
// it into the two things a thread shows: task updates as the work happens, and the answer as
// text. Nothing is invented and nothing is asked of the model: every payload was composed by
// the control plane from facts it already held, which is why there is no reasoning to leak here
// and nothing to sanitize.
//
// THE MODEL'S OWN REASONING NEVER APPEARS. The event stream carries none by construction, so a
// thread holds operational fact: what is being read, what it found, and the answer.

// Rendered is what one batch of events becomes in a thread.
type Rendered struct {
	// Status is the one-line "what is happening now" a thread shows above the answer while
	// the turn runs. Empty when this batch changed nothing about it.
	Status string
	// Text is answer text to append, already coalesced. Slack is never called per token:
	// the caller flushes on a size or interval boundary, and this is what has accumulated.
	Text string
	// Progress is completed reads, one line each, so the answer stays traceable in the
	// thread without a reader leaving it.
	Progress []string
	// Done reports that the turn reached a terminal state and the stream should be closed.
	Done bool
	// Failed reports that the terminal state was a failure. The thread says so plainly
	// rather than going quiet, because a question that got no answer and no explanation is
	// the worst outcome available.
	Failed bool
}

// Render maps events onto what a thread shows.
//
// Events arrive in order and each is rendered once, so a caller that advances its cursor by
// what it delivered cannot repeat itself. An event kind this build does not render is skipped
// rather than refused: a thread missing a line is a smaller failure than a delivery that stops.
func Render(events []investigation.Event) Rendered {
	var rendered Rendered
	var text strings.Builder

	for _, event := range events {
		switch event.Type {
		case investigation.EventStarted:
			rendered.Status = "Investigating"
		case investigation.EventProgress:
			if line := payloadText(event, "message", "text", "summary"); line != "" {
				rendered.Status = line
			}
		case investigation.EventToolStarted:
			if line := payloadText(event, "message", "tool", "name"); line != "" {
				rendered.Status = "Reading " + line
			}
		case investigation.EventToolCompleted:
			// One line per completed read, with what it found. This is what makes an
			// answer traceable in the thread rather than only in the console.
			if line := payloadText(event, "summary", "message", "tool", "name"); line != "" {
				rendered.Progress = append(rendered.Progress, line)
			}
		case investigation.EventAnswerDelta:
			// UNTRIMMED, unlike everything else here. A delta is a fragment of one
			// sentence, and the whitespace at its edges is the space between two words:
			// trimming it joins "The deploy at " to "14:02" as "The deploy at14:02".
			text.WriteString(payloadRaw(event, "text", "delta", "message"))
		case investigation.EventCompacted:
			// An internal memory decision. It is real and it is not something the person
			// who asked a question in Slack has any use for.
		case investigation.EventConcluded:
			rendered.Done = true
			rendered.Status = "Answered"
			// A conclusion may carry the whole answer where no deltas were streamed —
			// a cached or short turn — and appending it is what stops such a turn showing
			// a status and no answer.
			if text.Len() == 0 {
				text.WriteString(payloadText(event, "answer", "text", "message"))
			}
		case investigation.EventFailed:
			rendered.Done, rendered.Failed = true, true
			rendered.Status = "Could not finish"
			if reason := payloadText(event, "reason", "message", "error"); reason != "" {
				rendered.Progress = append(rendered.Progress, reason)
			}
		}
	}
	rendered.Text = text.String()
	return rendered
}

// payloadText reads the first of several keys that carries a non-empty string, trimmed, so a
// payload shape that gains a field does not silently render nothing. It is for the LINES a
// thread shows — a status, a completed read, a reason — each of which is a whole thing.
func payloadText(event investigation.Event, keys ...string) string {
	for _, key := range keys {
		if value, ok := event.Payload[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// payloadRaw reads the same keys and keeps the value exactly as it is. It is for answer text,
// where the edges of a fragment are the spaces between words.
func payloadRaw(event investigation.Event, keys ...string) string {
	for _, key := range keys {
		if value, ok := event.Payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// FailureNotice is what a thread is told when delivery itself could not finish.
//
// It is this build's own words. A vendor's message is text somebody else chose, and repeating
// it into a customer's workspace would put attacker-influenced text in front of a person.
const FailureNotice = "OpenCluster could not finish answering here. " +
	"The investigation itself is unaffected and is readable in the console."

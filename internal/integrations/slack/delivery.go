package slack

import (
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type Rendered struct {
	Status   string
	Text     string
	Progress []string
	Done     bool
	Failed   bool
	Footer   string
}

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
			if line := payloadText(event, "summary", "message", "tool", "name"); line != "" {
				rendered.Progress = append(rendered.Progress, line)
			}
		case investigation.EventConcluded:
			rendered.Done = true
			rendered.Status = "Answered"
			text.WriteString(payloadText(event, "summary"))
		case investigation.EventFailed:
			rendered.Done, rendered.Failed = true, true
			rendered.Status = "Could not finish"
			if reason := payloadText(event, "reason", "message", "error"); reason != "" {
				rendered.Progress = append(rendered.Progress, reason)
			}
		case investigation.EventCancelled:
			rendered.Done = true
			rendered.Status = "Cancelled"
			if reason := payloadText(event, "message"); reason != "" {
				rendered.Progress = append(rendered.Progress, reason)
			}
		}
	}
	rendered.Text = text.String()
	return rendered
}

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

const FailureNotice = "OpenCluster could not finish answering here. " +
	"The investigation itself is unaffected and is readable in the console."

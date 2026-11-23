package investigation

import (
	"strings"

	"github.com/google/uuid"
)

type EventPayload interface {
	EventType() EventType
}

type ToolStartedEventPayload struct {
	ToolRunID     uuid.UUID `json:"toolRunId"`
	Tool          string    `json:"tool"`
	IntegrationID string    `json:"integrationId"`
	Integration   string    `json:"integration"`
	Purpose       string    `json:"purpose"`
}

func (ToolStartedEventPayload) EventType() EventType { return EventToolStarted }

func ToolStartedPayload(run ToolRun, integration, name string) ToolStartedEventPayload {
	return ToolStartedEventPayload{
		ToolRunID:     run.ID,
		Tool:          bounded(run.Tool, eventTextBound),
		IntegrationID: integration,
		Integration:   bounded(name, eventTextBound),
		Purpose:       bounded(run.Purpose, eventTextBound),
	}
}

type ToolCompletedEventPayload struct {
	ToolRunID  uuid.UUID `json:"toolRunId"`
	Outcome    string    `json:"outcome"`
	DurationMs int64     `json:"durationMs"`
	Summary    string    `json:"summary"`
	Truncated  bool      `json:"truncated"`
}

func (ToolCompletedEventPayload) EventType() EventType { return EventToolCompleted }

func ToolCompletedPayload(run ToolRun) ToolCompletedEventPayload {
	duration := run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	summary := strings.TrimSpace(run.Summary)
	if summary == "" {
		if run.Outcome == RunSucceeded {
			summary = "Tool completed successfully"
		} else if safeError := safeToolRunError(run.Error); safeError != "" {
			summary = safeError
		} else {
			summary = "Tool failed"
		}
	}
	return ToolCompletedEventPayload{
		ToolRunID:  run.ID,
		Outcome:    outcomeWord(run.Outcome),
		DurationMs: duration,
		Summary:    bounded(summary, eventTextBound),
		Truncated:  run.Truncated,
	}
}

func safeToolRunError(reason string) string {
	reason = strings.TrimSpace(reason)
	switch reason {
	case "not one of the tools the selected sources offer",
		"the integration's credential could not be opened":
		return reason
	}
	return ""
}

type ConcludedEventPayload struct {
	Status  ConclusionStatus `json:"status,omitempty"`
	Summary string           `json:"summary"`
}

func (ConcludedEventPayload) EventType() EventType { return EventConcluded }

func ConcludedPayload(conclusion Conclusion) ConcludedEventPayload {
	return ConcludedEventPayload{
		Status:  conclusion.Status,
		Summary: bounded(conclusion.Summary, MaxSummaryLength),
	}
}

type FailedEventPayload struct {
	Reason string `json:"reason"`
}

func (FailedEventPayload) EventType() EventType { return EventFailed }

func FailedPayload(reason string) FailedEventPayload {
	return FailedEventPayload{
		Reason: bounded(reason, maxRunErrorLength),
	}
}

type CancelledEventPayload struct {
	Message string `json:"message"`
}

func (CancelledEventPayload) EventType() EventType { return EventCancelled }

func CancelledPayload() CancelledEventPayload {
	return CancelledEventPayload{
		Message: "Investigation cancelled by an operator",
	}
}

package investigation

import (
	"time"

	"github.com/google/uuid"
)

// EventPayload ties a current writer's payload to its wire event identity.
type EventPayload interface {
	EventType() EventType
}

type StartedEventPayload struct {
	Subject     string `json:"subject"`
	State       string `json:"state"`
	WindowFrom  string `json:"windowFrom"`
	WindowUntil string `json:"windowUntil"`
	Question    string `json:"question,omitempty"`
	Turn        int    `json:"turn,omitempty"`
}

func (StartedEventPayload) EventType() EventType { return EventStarted }

func StartedPayload(opened Investigation, executing bool) StartedEventPayload {
	payload := StartedEventPayload{
		Subject: bounded(opened.Subject, eventTextBound), State: "waiting",
		WindowFrom:  opened.WindowFrom.UTC().Format(time.RFC3339Nano),
		WindowUntil: opened.WindowUntil.UTC().Format(time.RFC3339Nano),
		Question:    bounded(opened.Question, eventTextBound), Turn: opened.Turn,
	}
	if executing {
		payload.State = "executing"
	}
	return payload
}

type ProgressEventPayload struct {
	Text string `json:"text"`
}

func (ProgressEventPayload) EventType() EventType { return EventProgress }

func ProgressPayload(text string) ProgressEventPayload {
	return ProgressEventPayload{Text: bounded(text, eventTextBound)}
}

type ToolStartedEventPayload struct {
	Ordinal       int            `json:"ordinal"`
	Tool          string         `json:"tool"`
	IntegrationID string         `json:"integrationId"`
	Integration   string         `json:"integration,omitempty"`
	Arguments     map[string]any `json:"arguments"`
	Purpose       string         `json:"purpose,omitempty"`
	HypothesisID  string         `json:"hypothesisId,omitempty"`
}

func (ToolStartedEventPayload) EventType() EventType { return EventToolStarted }

func ToolStartedPayload(run ToolRun, integration, name string) ToolStartedEventPayload {
	return ToolStartedEventPayload{
		Ordinal: run.Ordinal, Tool: bounded(run.Tool, eventTextBound), IntegrationID: integration,
		Integration: bounded(name, eventTextBound), Arguments: run.Arguments,
		Purpose: bounded(run.Purpose, eventTextBound), HypothesisID: bounded(run.HypothesisID, eventTextBound),
	}
}

type ToolCompletedEventPayload struct {
	Ordinal       int      `json:"ordinal"`
	Tool          string   `json:"tool"`
	Outcome       string   `json:"outcome"`
	DurationMs    int64    `json:"durationMs"`
	IntegrationID string   `json:"integrationId,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	Error         string   `json:"error,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
	WindowFrom    string   `json:"windowFrom,omitempty"`
	WindowUntil   string   `json:"windowUntil,omitempty"`
	Sources       []string `json:"sources,omitempty"`
}

func (ToolCompletedEventPayload) EventType() EventType { return EventToolCompleted }

func ToolCompletedPayload(run ToolRun) ToolCompletedEventPayload {
	payload := ToolCompletedEventPayload{
		Ordinal: run.Ordinal, Tool: bounded(run.Tool, eventTextBound), Outcome: outcomeWord(run.Outcome),
		DurationMs: run.FinishedAt.Sub(run.StartedAt).Milliseconds(), Summary: bounded(run.Summary, eventTextBound),
		Error: bounded(run.Error, eventTextBound), Truncated: run.Truncated, Sources: run.Sources,
	}
	if run.IntegrationID != uuid.Nil {
		payload.IntegrationID = run.IntegrationID.String()
	}
	if run.WindowApplied {
		payload.WindowFrom = run.WindowFrom.UTC().Format(time.RFC3339)
		payload.WindowUntil = run.WindowUntil.UTC().Format(time.RFC3339)
	}
	return payload
}

type HypothesesEventPayload struct {
	Version    int                `json:"version"`
	Hypotheses []HypothesisResult `json:"hypotheses"`
}

func (HypothesesEventPayload) EventType() EventType { return EventHypothesesUpdated }

func HypothesesUpdatedPayload(hypotheses []HypothesisResult) HypothesesEventPayload {
	return HypothesesEventPayload{Version: EventSchemaVersion, Hypotheses: hypotheses}
}

type ConcludedEventPayload struct {
	Status    ConclusionStatus `json:"status"`
	Findings  int              `json:"findings"`
	Summary   string           `json:"summary,omitempty"`
	StoppedBy string           `json:"stoppedBy,omitempty"`
}

func (ConcludedEventPayload) EventType() EventType { return EventConcluded }

func ConcludedPayload(conclusion Conclusion, stoppedBy string) ConcludedEventPayload {
	return ConcludedEventPayload{
		Status: conclusion.Status, Findings: len(conclusion.Findings),
		Summary: bounded(conclusion.Summary, MaxSummaryLength), StoppedBy: stoppedBy,
	}
}

type FailedEventPayload struct {
	Reason string `json:"reason"`
}

func (FailedEventPayload) EventType() EventType { return EventFailed }

func FailedPayload(reason string) FailedEventPayload {
	return FailedEventPayload{Reason: bounded(reason, maxRunErrorLength)}
}

type CancelledEventPayload struct {
	Message string `json:"message"`
}

func (CancelledEventPayload) EventType() EventType { return EventCancelled }

func CancelledPayload() CancelledEventPayload {
	return CancelledEventPayload{Message: "Investigation cancelled by an operator"}
}

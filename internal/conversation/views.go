package conversation

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type errorView struct {
	Error string `json:"error"`
}

type conversationView struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Source         string `json:"source"`
	IncidentID     string `json:"incidentId,omitempty"`
	CreatedBy      string `json:"createdBy,omitempty"`
	CreatedAt      string `json:"createdAt"`
	LastActivityAt string `json:"lastActivityAt"`
}

type messageView struct {
	WindowFrom      string    `json:"windowFrom"`
	WindowUntil     string    `json:"windowUntil"`
	Sequence        int64     `json:"sequence"`
	Role            string    `json:"role"`
	AuthorID        string    `json:"authorId,omitempty"`
	AuthorName      string    `json:"authorName,omitempty"`
	Text            string    `json:"text"`
	SourceReference string    `json:"sourceReference,omitempty"`
	InvestigationID string    `json:"investigationId,omitempty"`
	ExecutionID     uuid.UUID `json:"executionId,omitzero"`
	CreatedAt       string    `json:"createdAt"`
}

type turnView struct {
	InvestigationID string `json:"investigationId"`
	Turn            int    `json:"turn"`
	Status          string `json:"status"`
	StoppedBy       string `json:"stoppedBy,omitempty"`
	Error           string `json:"error,omitempty"`
	CreatedAt       string `json:"createdAt"`
	ConcludedAt     string `json:"concludedAt,omitempty"`
}

type messageAcceptedView struct {
	Message     messageView `json:"message"`
	ExecutionID uuid.UUID   `json:"executionId"`
	Status      string      `json:"status"`
	EventsURL   string      `json:"eventsUrl"`
}

func conversationViewOf(conversation Conversation) conversationView {
	view := conversationView{
		ID:             conversation.ID.String(),
		Title:          conversation.Title,
		Source:         conversation.Source.String(),
		CreatedAt:      stamp(conversation.CreatedAt),
		LastActivityAt: stamp(conversation.LastActivityAt),
		CreatedBy:      conversation.CreatedBy,
	}
	if conversation.IncidentID != uuid.Nil {
		view.IncidentID = conversation.IncidentID.String()
	}
	return view
}

func messageViewOf(message Message) messageView {
	view := messageView{
		Sequence:        message.Sequence,
		Role:            message.Role.String(),
		AuthorID:        message.AuthorID,
		AuthorName:      message.AuthorName,
		Text:            message.Text,
		ExecutionID:     message.ExecutionID,
		SourceReference: message.SourceReference,
		CreatedAt:       stamp(message.CreatedAt),
	}
	if message.InvestigationID != uuid.Nil {
		view.InvestigationID = message.InvestigationID.String()
	}
	if !message.WindowFrom.IsZero() {
		view.WindowFrom = message.WindowFrom.UTC().Format(time.RFC3339Nano)
		view.WindowUntil = message.WindowUntil.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func turnViewOf(turn Turn) turnView {
	view := turnView{
		InvestigationID: turn.InvestigationID.String(),
		Turn:            turn.Ordinal,
		Status:          turn.Status,
		StoppedBy:       turn.StoppedBy,
		Error:           turn.Error,
		CreatedAt:       stamp(turn.CreatedAt),
	}
	if !turn.ConcludedAt.IsZero() {
		view.ConcludedAt = stamp(turn.ConcludedAt)
	}
	return view
}

func stamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

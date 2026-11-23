package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type EventType int16

const (
	EventStarted       EventType = 1
	EventProgress      EventType = 2
	EventToolStarted   EventType = 3
	EventToolCompleted EventType = 4
	EventConcluded     EventType = 6
	EventFailed        EventType = 7
	EventCancelled     EventType = 9
)

func (t EventType) String() string {
	switch t {
	case EventStarted:
		return "started"
	case EventProgress:
		return "progress"
	case EventToolStarted:
		return "tool_started"
	case EventToolCompleted:
		return "tool_completed"
	case EventConcluded:
		return "concluded"
	case EventFailed:
		return "failed"
	case EventCancelled:
		return "cancelled"
	default:
		return "unrecognised"
	}
}

func (t EventType) Terminal() bool {
	return t == EventConcluded || t == EventFailed || t == EventCancelled
}

const EventSchemaVersion = 2

type Event struct {
	Sequence int64
	At       time.Time
	Type     EventType
	Payload  map[string]any
}

const (
	eventTextBound    = 512
	maxRunErrorLength = 1024
)

func bounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

const (
	eventPollInterval   = 500 * time.Millisecond
	eventHeartbeat      = 15 * time.Second
	eventWriteTimeout   = 10 * time.Second
	eventStreamLifetime = 31 * time.Minute
	eventAuthInterval   = 30 * time.Second
	eventAuthTimeout    = 5 * time.Second
)

func (h Handlers) streamEvents(writer http.ResponseWriter, request *http.Request) {
	if h.StreamContext != nil {
		ctx, cancel := context.WithCancel(request.Context())
		defer cancel()
		stop := context.AfterFunc(h.StreamContext, cancel)
		defer stop()
		request = request.WithContext(ctx)
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	if h.Store == nil || h.Resolve == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "this deployment does not serve the investigation event stream"})
		return
	}

	after, valid := afterSequence(request)
	if !valid {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "after is not a sequence"})
		return
	}

	nextAuth := time.Now().Add(eventAuthInterval)
	readCtx, cancelRead := context.WithTimeout(request.Context(), readTimeout)
	found, err := h.Store.WebInvestigation(readCtx, organization, id)
	cancelRead()
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	controller := http.NewResponseController(writer)
	if err := controller.SetWriteDeadline(time.Now().Add(eventWriteTimeout)); err != nil {
		writeJSON(writer, http.StatusInternalServerError,
			errorView{Error: "request failed"})
		h.Logger.ErrorContext(request.Context(),
			"the event stream is mounted behind a writer without write deadlines")
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return
	}
	defer func() { _ = controller.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) }()

	h.follow(request, writer, controller, organization, found, after, nextAuth)
}

func (h Handlers) follow(
	request *http.Request, writer http.ResponseWriter, controller *http.ResponseController,
	organization uuid.UUID, found Investigation, after int64, nextAuth time.Time,
) {
	ctx := request.Context()
	deadline := time.Now().Add(eventStreamLifetime)
	lastHeartbeat := time.Now()
	original := h.caller(request)
	stillAuthorized := func() bool {
		now := time.Now()
		if ctx.Err() != nil || !now.Before(deadline) {
			return false
		}
		if now.Before(nextAuth) {
			return true
		}
		nextAuth = now.Add(eventAuthInterval)
		check, cancel := context.WithTimeout(ctx, eventAuthTimeout)
		defer cancel()
		current, err := h.Resolve(request.WithContext(check))
		return err == nil && check.Err() == nil && time.Now().Before(deadline) &&
			current.UserID() == original.UserID() && current.SessionID() == original.SessionID() &&
			current.Organization() == organization && current.HavePermission(authz.InvestigationRead)
	}

	for {
		if !stillAuthorized() {
			return
		}
		readCtx, cancel := context.WithTimeout(request.Context(), readTimeout)
		events, err := h.Store.WebEvents(readCtx, organization, found.ID, after, 0)
		cancel()
		if err != nil {
			h.Logger.ErrorContext(ctx, "an investigation event stream could not be read",
				slog.String("investigation_id", found.ID.String()),
				slog.String("error", err.Error()))
			return
		}
		if !stillAuthorized() {
			return
		}
		if len(events) == 0 {
			if found.Status != StatusRunning {
				return
			}
			readCtx, cancel = context.WithTimeout(ctx, readTimeout)
			found, err = h.Store.WebInvestigation(readCtx, organization, found.ID)
			cancel()
			if err != nil || !stillAuthorized() {
				return
			}
			if found.Status != StatusRunning {
				continue
			}
		}

		if len(events) > 0 {
			if err := controller.SetWriteDeadline(time.Now().Add(eventWriteTimeout)); err != nil {
				return
			}
		}
		for _, event := range events {
			if err := writeEvent(writer, envelopeOf(organization, found, event)); err != nil {
				return
			}
			after = event.Sequence
			if event.Type.Terminal() {
				_ = controller.Flush()
				return
			}
		}
		if len(events) > 0 {
			if err := controller.Flush(); err != nil {
				return
			}
			lastHeartbeat = time.Now()
			if len(events) == maxEventsPerRead {
				continue
			}
		}

		if time.Since(lastHeartbeat) >= eventHeartbeat {
			if err := controller.SetWriteDeadline(time.Now().Add(eventWriteTimeout)); err != nil {
				return
			}
			if _, err := fmt.Fprint(writer, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			lastHeartbeat = time.Now()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(eventPollInterval):
		}
	}
}

const maxEventsPerRead = 500

type eventEnvelope struct {
	SchemaVersion   int            `json:"schemaVersion"`
	OrganizationID  string         `json:"organizationId"`
	ConversationID  string         `json:"conversationId,omitempty"`
	InvestigationID string         `json:"investigationId"`
	Sequence        int64          `json:"sequence"`
	Type            string         `json:"type"`
	At              string         `json:"at"`
	Payload         map[string]any `json:"payload"`
}

func envelopeOf(
	organization uuid.UUID, found Investigation, event Event,
) eventEnvelope {
	envelope := eventEnvelope{
		SchemaVersion:   EventSchemaVersion,
		OrganizationID:  organization.String(),
		InvestigationID: found.ID.String(),
		Sequence:        event.Sequence,
		Type:            event.Type.String(),
		At:              event.At.UTC().Format(time.RFC3339Nano),
		Payload:         event.Payload,
	}
	if found.ConversationID != uuid.Nil {
		envelope.ConversationID = found.ConversationID.String()
	}
	return envelope
}

func writeEvent(writer http.ResponseWriter, envelope eventEnvelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n",
		envelope.Sequence, envelope.Type, body)
	return err
}

func afterSequence(request *http.Request) (int64, bool) {
	value := request.URL.Query().Get("after")
	if value == "" {
		value = request.Header.Get("Last-Event-ID")
	}
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

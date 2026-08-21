package investigation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE EVENT STREAM AS A SURFACE.
//
// Server-sent events over plain HTTP, and no WebSocket. The traffic is one-directional —
// the platform tells a reader what happened — and every command this product takes is
// already an ordinary request. A bidirectional transport would be a second protocol to
// authorize, proxy and debug for a direction nothing uses.
//
// Replay and follow are ONE path. A reader asks for everything after a sequence it already
// has; that is answered from the table, and only then does the connection start following.
// So a first connection, a reconnect after a dropped socket, and a connection that landed
// on a different replica are the same code, which is the only way all three stay correct.

const (
	// eventPollInterval is how often a following connection looks for more. There is no
	// pub/sub here on purpose: a poll against a primary-key range scan is cheap, and the
	// alternative is an infrastructure dependency for a latency nobody is measuring.
	eventPollInterval = 500 * time.Millisecond
	// eventHeartbeat keeps an idle connection from being reaped by whatever sits in front
	// of it. A comment line is not an event and no reader has to know about it.
	eventHeartbeat = 15 * time.Second
	// eventStreamLifetime bounds one connection. It is the investigation's own ceiling
	// plus room to see the ending: a stream that outlived every possible investigation
	// would be a connection held open for nothing.
	eventStreamLifetime = investigationTimeout + time.Minute
)

// streamEvents serves one investigation's events.
//
// The tenant check happens FIRST, by reading the investigation itself. An identifier from
// another organization answers not-found with the same body as one that never existed,
// before a single event is read — the boundary must not depend on the stream being empty.
func (h Handlers) streamEvents(writer http.ResponseWriter, request *http.Request) {
	_, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	if h.Events == nil {
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

	readCtx, cancelRead := contextWithTimeout(request, readTimeout)
	found, err := h.Store.Investigation(readCtx, organization, id)
	cancelRead()
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	flusher, streaming := writer.(http.Flusher)
	if !streaming {
		// Without flushing, every event would arrive at once when the handler returned,
		// which is the opposite of what this is for.
		writeJSON(writer, http.StatusInternalServerError,
			errorView{Error: "request failed"})
		h.Logger.ErrorContext(request.Context(),
			"the event stream is mounted behind a writer that cannot flush")
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	// Named for the reverse proxies that buffer by default and turn a live stream into one
	// silent minute followed by everything.
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	h.follow(request, writer, flusher, organization, found, after)
}

// follow drains what is already recorded and then keeps draining until the investigation
// ends, the connection goes, or the lifetime is up.
func (h Handlers) follow(
	request *http.Request, writer http.ResponseWriter, flusher http.Flusher,
	organization tenancy.Organization, found Investigation, after int64,
) {
	ctx := request.Context()
	deadline := time.Now().Add(eventStreamLifetime)
	lastHeartbeat := time.Now()

	for {
		readCtx, cancel := contextWithTimeout(request, readTimeout)
		events, err := h.Events.Events(readCtx, organization, found.ID, after, 0)
		cancel()
		if err != nil {
			h.Logger.ErrorContext(ctx, "an investigation event stream could not be read",
				slog.String("investigation_id", found.ID.String()),
				slog.String("error", err.Error()))
			return
		}

		for _, event := range events {
			if err := writeEvent(writer, envelopeOf(organization, found, event)); err != nil {
				// The reader is gone. That is the ordinary way a stream ends.
				return
			}
			after = event.Sequence
			if event.Type.Terminal() {
				// Nothing follows a terminal event, so the connection has served its whole
				// purpose. Holding it open would be a client waiting for something this
				// investigation will never produce.
				flusher.Flush()
				return
			}
		}
		if len(events) > 0 {
			flusher.Flush()
			lastHeartbeat = time.Now()
			// A full page means there is probably more waiting; read again rather than
			// sleeping through a backlog.
			if len(events) == maxEventsPerRead {
				continue
			}
		}

		if time.Since(lastHeartbeat) >= eventHeartbeat {
			if _, err := fmt.Fprint(writer, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			lastHeartbeat = time.Now()
		}

		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(eventPollInterval):
		}
	}
}

// maxEventsPerRead mirrors persistence's own page bound, so the follower can tell a full
// page from a drained one without asking.
const maxEventsPerRead = 500

// eventEnvelope is the wire shape. The schema version lives HERE rather than in a column,
// because the table's shape is version 1 and a column would record the same number on every
// row forever. The identifiers travel on every event so that a reader multiplexing several
// streams never has to remember which connection it read from.
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
	organization tenancy.Organization, found Investigation, event Event,
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

// writeEvent renders one event in the SSE framing. The id is the sequence, so a browser's
// own EventSource reconnect sends Last-Event-ID and resumes exactly where it stopped —
// which is the same resume the `after` parameter serves.
func writeEvent(writer http.ResponseWriter, envelope eventEnvelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n",
		envelope.Sequence, envelope.Type, body)
	return err
}

// afterSequence reads the resume point from the query, or from the browser's own
// Last-Event-ID header when a native EventSource reconnected. Absent is from the
// beginning; anything unreadable is refused rather than treated as the beginning, because
// silently replaying a whole investigation is not what a resuming client asked for.
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

package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type EventType int16

const (
	EventStarted           EventType = 1
	EventProgress          EventType = 2
	EventToolStarted       EventType = 3
	EventToolCompleted     EventType = 4
	EventConcluded         EventType = 6
	EventFailed            EventType = 7
	EventCancelled         EventType = 9
	EventHypothesesUpdated EventType = 10
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
	case EventHypothesesUpdated:
		return "hypotheses_updated"
	default:
		return "unrecognised"
	}
}

// Terminal reports whether nothing follows this event for its investigation.
func (t EventType) Terminal() bool {
	return t == EventConcluded || t == EventFailed || t == EventCancelled
}

// EventSchemaVersion travels in the wire envelope rather than in a persisted column,
// because the table's shape IS version 1: a reader needs to know what it is being handed,
// and a column would record the same number on every row forever.
const EventSchemaVersion = 1

// Event is one thing that happened, at its position in the investigation.
type Event struct {
	// Sequence is monotonic within the investigation, from one. A reader that reconnects
	// asks for what comes after the number it already has, so this is the whole of the
	// resume contract.
	Sequence int64
	At       time.Time
	Type     EventType
	// Payload is the event's own structure. It never carries a credential, a header, a
	// system prompt or a raw tool result.
	Payload map[string]any
}

// eventTextBound caps any single string a payload carries. Progress lines are composed
// here and are short by construction; the bound is what stops a provider's own summary
// from being the exception.
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

type stream struct {
	appendEvent   func(context.Context, tenancy.Organization, uuid.UUID, Event) error
	organization  tenancy.Organization
	investigation uuid.UUID
	telemetry     *Telemetry
	// startedAt is when this stream began, which is when the run did.
	startedAt time.Time
	mu        sync.Mutex
	sequence  int64
	// sawFirst makes the time-to-first measurement happen once.
	sawFirst bool
	// closed is set by the terminal event. After it, nothing more is written for this
	// investigation — a reader that saw a terminal event may stop, and an event arriving
	// afterwards would mean it stopped too early.
	closed bool
}

// EventStream writes sanitized semantic progress for one Investigation.
type EventStream = stream

func NewEventStream(appendEvent func(
	context.Context,
	tenancy.Organization,
	uuid.UUID, Event) error,
	telemetry *Telemetry,
	organization tenancy.Organization,
	investigation uuid.UUID,
) *EventStream {
	return newStream(appendEvent, telemetry, organization, investigation)
}

func newStream(appendEvent func(
	context.Context,
	tenancy.Organization,
	uuid.UUID,
	Event) error,
	telemetry *Telemetry,
	organization tenancy.Organization,
	investigation uuid.UUID,
) *stream {
	return &stream{
		appendEvent:   appendEvent,
		telemetry:     telemetry,
		organization:  organization,
		investigation: investigation,
		startedAt:     time.Now(),
	}
}

func (s *stream) Emit(
	ctx context.Context, eventType EventType, payload map[string]any,
) error {
	return s.emit(ctx, eventType, payload)
}

// emit writes one event, returning whatever went wrong so the caller can log it.
func (s *stream) emit(
	ctx context.Context, eventType EventType, payload map[string]any,
) error {
	if s == nil || s.appendEvent == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.sequence++
	event := Event{
		Sequence: s.sequence,
		At:       time.Now().UTC(),
		Type:     eventType,
		Payload:  safePayload(payload),
	}
	if eventType.Terminal() {
		s.closed = true
	}
	// Measured inside the lock so "first" is decided once, and reported outside it so a
	// meter cannot hold up the writer.
	firstEvent := !s.sawFirst
	s.sawFirst = true
	s.mu.Unlock()

	if firstEvent {
		s.telemetry.firstEvent(time.Since(s.startedAt))
	}
	return s.appendEvent(ctx, s.organization, s.investigation, event)
}

// safePayload drops credential-shaped keys, mechanically, by the SAME rule the audit path
// applies — audit.NamesACredential is the one list, and a second copy of it would be a
// second place for one of the words to be missing.
func safePayload(payload map[string]any) map[string]any {
	safe := make(map[string]any, len(payload))
	if len(payload) == 0 {
		return safe
	}
	for _, key := range sortedKeys(payload) {
		if audit.NamesACredential(key) {
			continue
		}
		if len(safe) >= maxPayloadEntries {
			break
		}
		safe[key] = safeValue(payload[key])
	}
	return safe
}

// safeValue applies the same rule one level down and bounds what it finds there.
//
// The nested case is tool arguments, which is the one part of a payload the model wrote.
// They are bounded HERE rather than by whoever built the payload, because nothing upstream
// knows how long a value a model will invent.
func safeValue(value any) any {
	nested, isMap := value.(map[string]any)
	if !isMap {
		return value
	}
	inner := make(map[string]any, len(nested))
	for _, key := range sortedKeys(nested) {
		if audit.NamesACredential(key) {
			continue
		}
		if len(inner) >= maxPayloadEntries {
			break
		}
		if text, isText := nested[key].(string); isText {
			inner[key] = bounded(text, eventTextBound)
			continue
		}
		inner[key] = nested[key]
	}
	return inner
}

// sortedKeys orders a payload's keys so that dropping the overflow keeps the same entries
// every time. Map iteration order is random in Go, and one event keeping a field that the
// next one drops is a difference nobody can explain.
func sortedKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

const maxPayloadEntries = audit.MaxDetailEntries

// startedPayload opens the stream: what this investigation is about, and whether it is
// executing or still waiting. The lease is what distinguishes those, and both are
// `running`, so the first event is where a reader learns which.
func startedPayload(opened Investigation, executing bool) map[string]any {
	payload := map[string]any{
		"subject":     bounded(opened.Subject, eventTextBound),
		"state":       "waiting",
		"windowFrom":  opened.WindowFrom.UTC().Format(time.RFC3339),
		"windowUntil": opened.WindowUntil.UTC().Format(time.RFC3339),
	}
	if executing {
		payload["state"] = "executing"
	}
	if opened.Question != "" {
		payload["question"] = bounded(opened.Question, eventTextBound)
	}
	if opened.Turn > 0 {
		payload["turn"] = opened.Turn
	}
	return payload
}

// toolStartedPayload says which read is about to happen and where it is going. The
// arguments are the call's own scope, normalized: a person watching wants to know it is
// reading #deploys and not #random.
func toolStartedPayload(run ToolRun, integration string) map[string]any {
	payload := map[string]any{
		"ordinal":       run.Ordinal,
		"tool":          bounded(run.Tool, eventTextBound),
		"integrationId": integration,
		"arguments":     run.Arguments,
	}
	if run.Purpose != "" {
		payload["purpose"] = bounded(run.Purpose, eventTextBound)
	}
	if run.HypothesisID != "" {
		payload["hypothesisId"] = bounded(run.HypothesisID, eventTextBound)
	}
	return payload
}

// toolCompletedPayload says what came back, in one line, using the provider's OWN summary
// — the same sentence the provenance records. A failed read is reported as a failure
// rather than omitted, because a gap in an answer that is explained is a different thing
// from one that is silent.
func toolCompletedPayload(run ToolRun) map[string]any {
	payload := map[string]any{
		"ordinal":    run.Ordinal,
		"tool":       bounded(run.Tool, eventTextBound),
		"outcome":    outcomeWord(run.Outcome),
		"durationMs": run.FinishedAt.Sub(run.StartedAt).Milliseconds(),
	}
	if run.IntegrationID != uuid.Nil {
		payload["integrationId"] = run.IntegrationID.String()
	}
	if run.Summary != "" {
		payload["summary"] = bounded(run.Summary, eventTextBound)
	}
	if run.Error != "" {
		payload["error"] = bounded(run.Error, eventTextBound)
	}
	if run.Truncated {
		payload["truncated"] = true
	}
	// The window the read covered, for the reader who has to tell an empty window from an
	// empty estate. Absent on a read that covers none, because claiming a window a
	// repository listing never applied would answer the question wrongly rather than not
	// at all.
	if run.WindowApplied {
		payload["windowFrom"] = run.WindowFrom.UTC().Format(time.RFC3339)
		payload["windowUntil"] = run.WindowUntil.UTC().Format(time.RFC3339)
	}
	if len(run.Sources) > 0 {
		payload["sources"] = run.Sources
	}
	return payload
}

// progressPayload is a composed sentence and the fact behind it. The text is written here,
// from what the platform knows; it is never asked for and never received.
func progressPayload(text string) map[string]any {
	return map[string]any{"text": bounded(text, eventTextBound)}
}

func hypothesesUpdatedPayload(hypotheses []HypothesisResult) map[string]any {
	return map[string]any{
		"version":    HypothesisSnapshotVersion,
		"hypotheses": hypotheses,
	}
}

// concludedPayload is the ending a reader stops on: the direct answer, how much was
// established, and the ceiling that forced it if one did.
func concludedPayload(conclusion Conclusion, stoppedBy string) map[string]any {
	payload := map[string]any{
		"status":   conclusion.Status,
		"findings": len(conclusion.Findings),
	}
	if conclusion.Summary != "" {
		payload["summary"] = bounded(conclusion.Summary, MaxSummaryLength)
	}
	if stoppedBy != "" {
		payload["stoppedBy"] = stoppedBy
	}
	return payload
}

// failedPayload is the other ending. It always states a reason, because a reader left
// watching a spinner is the failure this exists to prevent.
func failedPayload(reason string) map[string]any {
	return map[string]any{"reason": bounded(reason, maxRunErrorLength)}
}

func StartedPayload(opened Investigation, executing bool) map[string]any {
	return startedPayload(opened, executing)
}

func ToolStartedPayload(run ToolRun, integration string) map[string]any {
	return toolStartedPayload(run, integration)
}

func ToolCompletedPayload(run ToolRun) map[string]any { return toolCompletedPayload(run) }

func ProgressPayload(text string) map[string]any { return progressPayload(text) }

func HypothesesUpdatedPayload(hypotheses []HypothesisResult) map[string]any {
	return hypothesesUpdatedPayload(hypotheses)
}

func ConcludedPayload(conclusion Conclusion, stoppedBy string) map[string]any {
	return concludedPayload(conclusion, stoppedBy)
}

func FailedPayload(reason string) map[string]any { return failedPayload(reason) }

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
	if h.Store == nil {
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

	// Bounded, and still cancelled by the request's own context, so a reader that
	// disconnects stops the read it was waiting on.
	readCtx, cancelRead := context.WithTimeout(request.Context(), readTimeout)
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
		readCtx, cancel := context.WithTimeout(request.Context(), readTimeout)
		events, err := h.Store.Events(readCtx, organization, found.ID, after, 0)
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

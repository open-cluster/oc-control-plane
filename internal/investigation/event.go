package investigation

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE EVENT STREAM — what a reader sees while an investigation runs.
//
// An investigation used to be opaque until it ended: a surface could poll the running
// record and whatever provenance had been written, and "OpenCluster is reading commits for
// checkout-api" had nothing to travel on. These events are that thing.
//
// NO MODEL CHAIN OF THOUGHT EVER ENTERS ONE. Every payload is composed HERE, from facts
// the control plane already holds — which tool is about to run against which integration,
// what that tool's own summary said, which ceiling fired. No prompt asks the model to
// narrate, which is the cheapest possible answer to "never expose the reasoning": there is
// nothing to sanitize because nothing private is ever asked for.

// EventType is one kind of semantic fact. Persisted as the integer in the column; the
// values are frozen. Eight, and no more in this release — `question` is deliberately
// absent because this investigator cannot ask the operator anything, and adding it later
// is one value and one payload.
type EventType int16

const (
	EventStarted EventType = iota + 1
	EventProgress
	EventToolStarted
	EventToolCompleted
	EventAnswerDelta
	EventConcluded
	EventFailed
	EventCompacted
)

// EventTypes enumerates the legal values, for decoders and gates.
var EventTypes = []EventType{
	EventStarted, EventProgress, EventToolStarted, EventToolCompleted,
	EventAnswerDelta, EventConcluded, EventFailed, EventCompacted,
}

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
	case EventAnswerDelta:
		return "answer_delta"
	case EventConcluded:
		return "concluded"
	case EventFailed:
		return "failed"
	case EventCompacted:
		return "compacted"
	default:
		return "unrecognised"
	}
}

// Terminal reports whether nothing follows this event for its investigation.
func (t EventType) Terminal() bool { return t == EventConcluded || t == EventFailed }

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
const eventTextBound = 512

// EventSink is where events are written. It is declared here, in the domain's vocabulary,
// and implemented by storage.
type EventSink interface {
	// AppendEvent writes one event at the sequence it carries. The primary key refuses a
	// position twice, which is the backstop against a double-claim.
	AppendEvent(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		event Event) error
}

// EventReader is how a surface replays what has already happened.
type EventReader interface {
	// Events reports this investigation's events after a sequence, in order, bounded.
	// after of zero is from the beginning.
	Events(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		after int64, limit int) ([]Event, error)
}

// stream is one investigation's writer. The sequence counter is in-process because the
// LEASE is exclusive: there is exactly one writer per investigation, and the primary key
// catches the case where that turns out to be false.
//
// A write that fails is logged by the caller and does not fail the investigation. An event
// stream is how a run is watched, not what it is: losing a progress line is a worse
// experience, and failing the investigation for it would be a worse outcome.
type stream struct {
	sink          EventSink
	organization  tenancy.Organization
	investigation uuid.UUID
	telemetry     *Telemetry
	// startedAt is when this stream began, which is when the run did. The two
	// time-to-first measurements are taken from it.
	startedAt time.Time

	mu       sync.Mutex
	sequence int64
	// sawFirst and sawAnswer make the two time-to-first measurements happen once each,
	// which is what "first" means.
	sawFirst  bool
	sawAnswer bool
	// closed is set by the terminal event. After it, nothing more is written for this
	// investigation — a reader that saw a terminal event may stop, and an event arriving
	// afterwards would mean it stopped too early.
	closed bool
}

// newStream begins one investigation's event stream. A nil sink produces a stream that
// silently discards, which is what a test with no interest in events should get.
func newStream(
	sink EventSink, telemetry *Telemetry, organization tenancy.Organization,
	investigation uuid.UUID,
) *stream {
	return &stream{
		sink: sink, telemetry: telemetry, organization: organization,
		investigation: investigation, startedAt: time.Now(),
	}
}

// emit writes one event, returning whatever went wrong so the caller can log it.
func (s *stream) emit(
	ctx context.Context, eventType EventType, payload map[string]any,
) error {
	if s == nil || s.sink == nil {
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
	firstAnswer := eventType == EventAnswerDelta && !s.sawAnswer
	if firstAnswer {
		s.sawAnswer = true
	}
	s.mu.Unlock()

	if firstEvent {
		s.telemetry.firstEvent(time.Since(s.startedAt))
	}
	if firstAnswer {
		s.telemetry.firstAnswerText(time.Since(s.startedAt))
	}

	return s.sink.AppendEvent(ctx, s.organization, s.investigation, event)
}

// safePayload drops credential-shaped keys, mechanically, by the SAME rule the audit path
// applies — audit.NamesACredential is the one list, and a second copy of it would be a
// second place for one of the words to be missing.
//
// What it does NOT do is bound the strings. Each payload above bounds its own, because the
// right limit differs: a progress line is one sentence and the direct answer is a
// paragraph, and running everything through one number would silently truncate the answer
// to the length of a summary.
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

// maxPayloadEntries bounds one payload's breadth, mirroring the audit record's own cap. It
// exists for the nested tool arguments, which are the only part a model decides the shape
// of.
const maxPayloadEntries = audit.MaxDetailEntries

// The payloads, composed from held facts. Each is a function rather than a literal at the
// call site so that what a reader receives is decided in ONE place and can be read as a
// contract.

// startedPayload opens the stream: what this investigation is about, and whether it is
// executing or still waiting. The lease is what distinguishes those, and both are
// `running`, so the first event is where a reader learns which.
func startedPayload(opened Investigation, sources int, executing bool) map[string]any {
	payload := map[string]any{
		"subject":     bounded(opened.Subject, eventTextBound),
		"sources":     sources,
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
	return map[string]any{
		"ordinal":       run.Ordinal,
		"tool":          bounded(run.Tool, eventTextBound),
		"capability":    bounded(run.Capability, eventTextBound),
		"integrationId": integration,
		"arguments":     run.Arguments,
	}
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

// concludedPayload is the ending a reader stops on: the direct answer, how much was
// established, and the ceiling that forced it if one did.
func concludedPayload(conclusion Conclusion, stoppedBy string) map[string]any {
	payload := map[string]any{
		"findings": len(conclusion.Findings),
	}
	if conclusion.Answer != "" {
		payload["answer"] = bounded(conclusion.Answer, eventTextBound)
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

// answerDeltaPayload carries a coalesced checkpoint of the answer, never a token. Where a
// provider streams, deltas are buffered and flushed on a boundary; where it does not — and
// none does today — one chunk is emitted at conclusion. Individual token deltas are never
// persisted, because a table with one row per token is a table nobody can read.
func answerDeltaPayload(text string, final bool) map[string]any {
	return map[string]any{
		"text":  bounded(text, maxAnswerDeltaBound),
		"final": final,
	}
}

// maxAnswerDeltaBound lets one delta carry the whole answer, since today's providers
// deliver it at once.
const maxAnswerDeltaBound = MaxAnswerLength

// compactedPayload reports what compaction was worth and nothing about what it said. The
// summary text is internal: a reader learns that memory was consolidated and how much it
// saved, which is what tells them whether the layer is working.
func compactedPayload(version int, before, after int) map[string]any {
	return map[string]any{
		"version":      version,
		"tokensBefore": before,
		"tokensAfter":  after,
	}
}

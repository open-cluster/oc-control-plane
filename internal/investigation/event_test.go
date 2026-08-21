package investigation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE EVENT STREAM, WITH NO MODEL AND NO CHAT SURFACE.
//
// A scripted exchange emits a move sequence and a recording sink collects what came out.
// What is asserted is the stream's own contract — ordering, monotonicity, exactly one
// terminal event, nothing after it, a failed read represented as a failure, resume from an
// arbitrary point, and no model-authored text anywhere in progress. None of that needs a
// provider, so none of these tests pay one.

// recordingSink collects events in the order they were written.
type recordingSink struct {
	mu       sync.Mutex
	events   []Event
	failNext bool
}

func (r *recordingSink) AppendEvent(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, event Event,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("the event could not be written")
	}
	r.events = append(r.events, event)
	return nil
}

func (r *recordingSink) collected() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *recordingSink) ofType(wanted EventType) []Event {
	var found []Event
	for _, event := range r.collected() {
		if event.Type == wanted {
			found = append(found, event)
		}
	}
	return found
}

// runWatched drives one whole investigation with a recording sink attached.
func runWatched(
	t *testing.T, store *memoryStore, catalog integrations.Catalog,
	investigator Investigator,
) *recordingSink {
	t.Helper()

	sink := &recordingSink{}
	runner := &Runner{
		Store: store, Catalog: catalog, Investigator: investigator, Events: sink,
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency", Question: "what changed?",
		WindowFrom:  time.Now().Add(-2 * time.Hour),
		WindowUntil: time.Now(),
	})
	runner.running.Wait()
	return sink
}

// oneRead scripts an exchange that reads once and then concludes.
func oneRead() *scriptedExchange {
	return &scriptedExchange{moves: []Move{
		{Calls: []AgentCall{{ID: "call-1", Tool: "stub.read",
			Arguments: map[string]any{"channel": "deploys"}}}},
		{Conclusion: &Conclusion{
			Answer: "the deployed revision is v2.14.1",
			Findings: []Finding{{
				Statement: "the deployed revision is v2.14.1", Kind: FindingObservation,
				Confidence: ConfidenceConfirmed, Sources: []int{1},
			}},
		}},
	}}
}

// A whole run produces its events in order, at consecutive sequences, ending in exactly one
// terminal event with nothing after it.
func TestTheStreamIsOrderedMonotonicAndEndsExactlyOnce(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			Content: []string{"v2.14.1"}, Summary: "1 deploy", Sources: []string{"C1"},
		}, nil
	})

	sink := runWatched(t, store, catalog,
		&scriptedInvestigator{exchange: oneRead()})

	events := sink.collected()
	if len(events) < 4 {
		t.Fatalf("%d events; a read and a conclusion produce at least four", len(events))
	}
	for position, event := range events {
		if event.Sequence != int64(position+1) {
			t.Fatalf("event %d is at sequence %d; the sequence is monotonic from one and "+
				"a reader resumes by it", position, event.Sequence)
		}
		if event.At.IsZero() {
			t.Errorf("event %d carries no timestamp", event.Sequence)
		}
	}

	if events[0].Type != EventStarted {
		t.Errorf("the first event is %s, want started", events[0].Type)
	}

	terminals := 0
	for position, event := range events {
		if !event.Type.Terminal() {
			continue
		}
		terminals++
		if position != len(events)-1 {
			t.Errorf("event %d is terminal (%s) with %d events after it; nothing follows "+
				"a terminal event", event.Sequence, event.Type, len(events)-position-1)
		}
	}
	if terminals != 1 {
		t.Errorf("%d terminal events, want exactly one", terminals)
	}
	if events[len(events)-1].Type != EventConcluded {
		t.Errorf("the run ended with %s, want concluded", events[len(events)-1].Type)
	}
}

// The order of a read is start-then-complete, and the completion carries the provider's own
// one-line summary — the same sentence provenance records, so the two never disagree.
func TestAReadIsAnnouncedBeforeItRunsAndSummarisedAfter(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			Content: []string{"v2.14.1"}, Summary: "1 deploy", Sources: []string{"C1"},
		}, nil
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{exchange: oneRead()})

	started := sink.ofType(EventToolStarted)
	completed := sink.ofType(EventToolCompleted)
	if len(started) != 1 || len(completed) != 1 {
		t.Fatalf("%d started and %d completed, want one of each", len(started),
			len(completed))
	}
	if started[0].Sequence >= completed[0].Sequence {
		t.Errorf("the read completed (%d) before it started (%d)", completed[0].Sequence,
			started[0].Sequence)
	}
	if started[0].Payload["tool"] != "stub.read" {
		t.Errorf("the start event does not name the tool: %+v", started[0].Payload)
	}
	if started[0].Payload["integration"] != "Deploy Slack" {
		t.Errorf("the start event does not name the integration it reaches: %+v",
			started[0].Payload)
	}
	if completed[0].Payload["outcome"] != "succeeded" ||
		completed[0].Payload["summary"] != "1 deploy" {
		t.Errorf("the completion does not carry the provider's own summary: %+v",
			completed[0].Payload)
	}
}

// A read that failed is reported as a failure, not omitted. A gap in an answer that is
// explained is a different thing from one that is silent.
func TestAFailedReadIsRepresentedAsAFailure(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, errors.New("the channel is not readable")
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{moves: []Move{
			{Calls: []AgentCall{{ID: "call-1", Tool: "stub.read",
				Arguments: map[string]any{"channel": "deploys"}}}},
			{Conclusion: &Conclusion{Answer: "nothing could be read"}},
		}},
	})

	completed := sink.ofType(EventToolCompleted)
	if len(completed) != 1 {
		t.Fatalf("%d completions, want one", len(completed))
	}
	if completed[0].Payload["outcome"] != "failed" {
		t.Errorf("a read that failed reports outcome %v", completed[0].Payload["outcome"])
	}
	if text, _ := completed[0].Payload["error"].(string); text == "" {
		t.Errorf("a failed read states no reason: %+v", completed[0].Payload)
	}
}

// A failing investigation ends with a stated reason. A reader left watching a spinner
// forever is the failure the terminal event exists to prevent.
func TestAFailedInvestigationEndsTheStreamWithAReason(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{}, nil
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{failure: ErrReasonerUnavailable},
	})

	events := sink.collected()
	if len(events) == 0 {
		t.Fatal("a failing investigation emitted nothing at all")
	}
	last := events[len(events)-1]
	if last.Type != EventFailed {
		t.Fatalf("the stream ended with %s, want failed", last.Type)
	}
	if reason, _ := last.Payload["reason"].(string); reason == "" {
		t.Errorf("the failure states no reason: %+v", last.Payload)
	}
}

// NO MODEL-AUTHORED TEXT REACHES THE STREAM.
//
// Every progress line is composed by the platform from a fact it already holds. This drives
// an exchange whose every string is a marker and asserts that none of them appears
// anywhere in the stream — which also covers the model's own tool arguments being echoed
// into a sentence.
func TestNoModelAuthoredTextReachesTheStream(t *testing.T) {
	t.Parallel()

	const marker = "MODEL-PRIVATE-REASONING"

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{
			// A provider's own summary IS platform-held fact and is allowed through, so
			// the marker must not be here; the content, which is never streamed, carries
			// it instead.
			Content: []string{marker + " in the tool's raw content"},
			Summary: "1 deploy",
		}, nil
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{moves: []Move{
			{Calls: []AgentCall{{ID: "call-1", Tool: "stub.read"}}},
			{Conclusion: &Conclusion{
				Answer: "a plain answer",
				Findings: []Finding{{
					Statement:  marker + " stated as a finding",
					Kind:       FindingObservation,
					Confidence: ConfidenceConfirmed,
					Sources:    []int{1},
				}},
			}},
		}},
	})

	for _, event := range sink.ofType(EventProgress) {
		text, _ := event.Payload["text"].(string)
		if strings.Contains(text, marker) {
			t.Errorf("progress %d carries model-authored text: %q", event.Sequence, text)
		}
	}
	// Findings are not streamed as text either: the concluded event carries their COUNT,
	// and the record carries the statements.
	for _, event := range sink.collected() {
		for key, value := range event.Payload {
			text, isText := value.(string)
			if isText && strings.Contains(text, marker) {
				t.Errorf("event %d (%s) carries %q in %q; nothing the model wrote and no "+
					"raw tool payload belongs on this stream",
					event.Sequence, event.Type, key, text)
			}
		}
	}
}

// REDACTION. A tool whose arguments carry credential-shaped values must not put them on the
// stream. The rule is the audit path's own mechanical key-dropping, reused rather than
// re-listed, so there is one place a forbidden word can be missing rather than two.
func TestCredentialShapedArgumentsNeverReachTheStream(t *testing.T) {
	t.Parallel()

	const leaked = "xoxb-a-real-looking-token"

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{moves: []Move{
			{Calls: []AgentCall{{ID: "call-1", Tool: "stub.read", Arguments: map[string]any{
				"channel":       "deploys",
				"token":         leaked,
				"api_key":       leaked,
				"authorization": "Bearer " + leaked,
			}}}},
			{Conclusion: &Conclusion{Answer: "done"}},
		}},
	})

	started := sink.ofType(EventToolStarted)
	if len(started) != 1 {
		t.Fatalf("%d start events, want one", len(started))
	}
	arguments, ok := started[0].Payload["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("the start event carries no arguments: %+v", started[0].Payload)
	}
	if arguments["channel"] != "deploys" {
		t.Errorf("the scope a person needs to see was dropped: %+v; redaction must cost "+
			"context, not the whole field", arguments)
	}
	for _, forbidden := range []string{"token", "api_key", "authorization"} {
		if _, present := arguments[forbidden]; present {
			t.Errorf("the stream carries a %q argument: %+v", forbidden, arguments)
		}
	}
	for _, event := range sink.collected() {
		if strings.Contains(renderPayload(event.Payload), leaked) {
			t.Errorf("event %d (%s) carries a credential-shaped value: %+v",
				event.Sequence, event.Type, event.Payload)
		}
	}
}

// renderPayload flattens a payload to text, so a leak nested one level down is still found.
func renderPayload(payload map[string]any) string {
	var out strings.Builder
	for _, value := range payload {
		switch typed := value.(type) {
		case string:
			out.WriteString(typed + " ")
		case map[string]any:
			out.WriteString(renderPayload(typed))
		}
	}
	return out.String()
}

// A ceiling that ends the reads is announced in the platform's own words, and the run still
// concludes. "We stopped" is never dressed up as "we found nothing".
func TestAFiredCeilingIsAnnouncedAsProgress(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "nothing new"}, nil
	})

	// Two turns of reads that establish nothing new trip the stagnation guard.
	repeat := AgentCall{ID: "call-1", Tool: "stub.read",
		Arguments: map[string]any{"channel": "deploys"}}
	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{moves: []Move{
			{Calls: []AgentCall{repeat}},
			{Calls: []AgentCall{repeat}},
			{Calls: []AgentCall{repeat}},
			{Conclusion: &Conclusion{Answer: "stopped early"}},
		}},
	})

	progress := sink.ofType(EventProgress)
	if len(progress) == 0 {
		t.Fatal("no progress was announced; a reader learns why the reads ended here")
	}
	announced := false
	for _, event := range progress {
		if text, _ := event.Payload["text"].(string); strings.HasPrefix(text, "Stopping the reads") {
			announced = true
		}
	}
	if !announced {
		t.Errorf("no progress said the reads were over: %+v", progress)
	}
	if last := sink.collected(); last[len(last)-1].Type != EventConcluded {
		t.Errorf("a stopped investigation ended with %s, want concluded",
			last[len(last)-1].Type)
	}
}

// An event that cannot be written must not end the investigation. The record is the
// investigation; the stream is a view of it being produced, and trading a completed
// diagnosis for a missing progress line would be the wrong way round.
func TestAnUnwritableEventDoesNotFailTheInvestigation(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	sink := &recordingSink{failNext: true}
	runner := &Runner{
		Store: store, Catalog: catalog, Events: sink,
		Investigator: &scriptedInvestigator{exchange: oneRead()},
		Logger:       slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	if store.status != StatusConcluded {
		t.Errorf("the investigation is %v after one event failed to write; the stream is "+
			"how a run is watched, not what it is", store.status)
	}
}

// A stream with no sink is a stream that discards. An investigation must run identically
// whether or not anybody is watching.
func TestAnInvestigationRunsWithNoEventSinkAtAll(t *testing.T) {
	t.Parallel()

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	runAutonomous(t, store, catalog, &scriptedInvestigator{exchange: oneRead()})

	if store.status != StatusConcluded {
		t.Errorf("status = %v with no event sink configured", store.status)
	}
	if store.answer != "the deployed revision is v2.14.1" {
		t.Errorf("answer = %q", store.answer)
	}
}

// Nothing is written after a terminal event, even if something tries. The stream guards it
// itself rather than trusting every call site to check, because a reader that saw the
// ending has already stopped.
func TestNothingIsWrittenAfterATerminalEvent(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	events := newStream(sink, organization, uuid.New())

	for _, emitted := range []EventType{
		EventStarted, EventProgress, EventConcluded, EventProgress, EventFailed,
	} {
		if emitErr := events.emit(context.Background(), emitted, nil); emitErr != nil {
			t.Fatalf("emit(%s): %v", emitted, emitErr)
		}
	}

	collected := sink.collected()
	if len(collected) != 3 {
		t.Fatalf("%d events written, want three: everything after the terminal is dropped",
			len(collected))
	}
	if collected[2].Type != EventConcluded {
		t.Errorf("the last event is %s, want concluded", collected[2].Type)
	}
}

// The answer delta carries the WHOLE answer. Bounds differ per payload — a progress line is
// one sentence, the direct answer is a paragraph — and a single shared limit would silently
// cut the answer down to the length of a summary without anything saying so.
func TestTheAnswerDeltaCarriesTheWholeAnswer(t *testing.T) {
	t.Parallel()

	answer := strings.Repeat("a", eventTextBound*3)

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	sink := runWatched(t, store, catalog, &scriptedInvestigator{
		exchange: &scriptedExchange{moves: []Move{
			{Conclusion: &Conclusion{Answer: answer}},
		}},
	})

	deltas := sink.ofType(EventAnswerDelta)
	if len(deltas) != 1 {
		t.Fatalf("%d answer deltas, want one", len(deltas))
	}
	carried, _ := deltas[0].Payload["text"].(string)
	if carried != answer {
		t.Errorf("the answer delta carries %d characters of %d; a shared bound must not "+
			"truncate the answer to the length of a summary", len(carried), len(answer))
	}
	if final, _ := deltas[0].Payload["final"].(bool); !final {
		t.Errorf("the only delta is not marked final: %+v", deltas[0].Payload)
	}
}

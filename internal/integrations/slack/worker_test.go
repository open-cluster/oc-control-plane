package slack

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

type slackCallLog struct {
	*httptest.Server
	mu        sync.Mutex
	calls     []string
	text      []string
	streaming bool
	failFor   map[string]int
	names     map[string]string
}

func newSlackCallLog(t *testing.T, streaming bool) *slackCallLog {
	t.Helper()

	fake := &slackCallLog{
		streaming: streaming,
		failFor:   map[string]int{},
		names:     map[string]string{},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			method := strings.TrimPrefix(request.URL.Path, "/")
			_ = request.ParseForm()

			fake.mu.Lock()
			defer fake.mu.Unlock()

			writer.Header().Set("Content-Type", "application/json")
			if !fake.streaming && strings.HasSuffix(method, "Stream") {
				_, _ = writer.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
				return
			}
			if fake.failFor[method] > 0 {
				fake.failFor[method]--
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}

			if method == "users.info" {
				fake.calls = append(fake.calls, method)
				fake.text = append(fake.text, "")
				name, known := fake.names[request.FormValue("user")]
				if !known {
					_, _ = writer.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
					return
				}
				_, _ = writer.Write([]byte(`{"ok":true,"user":{"id":"x","name":"` + name +
					`","profile":{"display_name":"` + name + `","real_name":"` + name + `"}}}`))
				return
			}

			fake.calls = append(fake.calls, method)
			carried := request.PostFormValue("text")
			if carried == "" {
				carried = request.PostFormValue("markdown_text")
			}
			fake.text = append(fake.text, carried)
			_, _ = writer.Write([]byte(`{"ok":true,"ts":"1700000100.100"}`))
		}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *slackCallLog) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *slackCallLog) carried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.text...)
}

func (f *slackCallLog) name(id, display string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[id] = display
}

func (f *slackCallLog) failNext(method string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFor[method] = times
}

type repliesInMemory struct {
	mu        sync.Mutex
	reply     Reply
	events    []investigation.Event
	retries   []string
	audited   []string
	unnamed   []string
	named     map[string]string
	completed bool
	gaveUp    bool
	sealed    []byte
}

func (d *repliesInMemory) ClaimSlackReplies(
	context.Context, int, time.Duration,
) ([]Reply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.completed || d.gaveUp {
		return nil, nil
	}
	reply := d.reply
	reply.LeaseExpiresAt = time.Now().Add(leaseDuration)
	return []Reply{reply}, nil
}

func (d *repliesInMemory) ReleaseSlackReply(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

func (d *repliesInMemory) AdvanceSlackReply(
	_ context.Context, _ uuid.UUID, _, _ uuid.UUID, made Progress,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.reply.Stream.Held() {
		d.reply.Stream = made.Stream
	}
	if made.Sequence > d.reply.LastSequence {
		d.reply.LastSequence = made.Sequence
	}
	d.reply.Attempts = 0
	return nil
}

func (d *repliesInMemory) RecordCollaborationWrite(
	_ context.Context, _ uuid.UUID, _ uuid.UUID, where string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audited = append(d.audited, where)
	return nil
}

func (d *repliesInMemory) CompleteSlackReply(
	context.Context, uuid.UUID, uuid.UUID, uuid.UUID,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completed = true
	return nil
}

func (d *repliesInMemory) RetrySlackReply(
	_ context.Context, _ uuid.UUID, _, _ uuid.UUID,
	_ time.Time, note string, giveUp bool,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retries = append(d.retries, note)
	d.reply.Attempts++
	d.gaveUp = giveUp
	return nil
}

func (d *repliesInMemory) Integration(
	context.Context, uuid.UUID, uuid.UUID,
) (integrations.Integration, error) {
	return integrations.Integration{
		ID: d.reply.Integration, CredentialSealed: d.sealed,
	}, nil
}

func (d *repliesInMemory) Events(
	_ context.Context, _ uuid.UUID, _ uuid.UUID, after int64, limit int,
) ([]investigation.Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var found []investigation.Event
	for _, event := range d.events {
		if event.Sequence > after && len(found) < limit {
			found = append(found, event)
		}
	}
	return found, nil
}

func answering(t *testing.T, fake *slackCallLog, events []investigation.Event) (
	Worker, *repliesInMemory,
) {
	t.Helper()

	organization := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeyLength))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}
	integration := uuid.New()
	sealed, err := sealer.Seal("xoxb-under-test", integrations.CredentialBinding(integration))
	if err != nil {
		t.Fatalf("sealing the bot token: %v", err)
	}

	state := &repliesInMemory{
		reply: Reply{
			Investigation: uuid.New(), Organization: organization,
			Integration: integration,
			Stream:      Stream{Channel: "C0INCIDENTS", Thread: "1700000001.1"},
		},
		events: events,
		sealed: sealed,
	}
	return Worker{
		Replies: state,
		Client:  NewClient(fake.URL),
		Sealer:  sealer,
		Logger:  testLogger(t),
	}, state
}

func progressed(sequence int64, kind investigation.EventType, payload map[string]any,
) investigation.Event {
	return investigation.Event{Sequence: sequence, Type: kind, Payload: payload}
}

func aTurn() []investigation.Event {
	return []investigation.Event{
		progressed(1, investigation.EventStarted, nil),
		progressed(2, investigation.EventToolCompleted,
			map[string]any{"summary": "read 40 commits on checkout-api"}),
		progressed(3, investigation.EventProgress, map[string]any{"text": "Checking impact"}),
		progressed(4, investigation.EventConcluded,
			map[string]any{"summary": "The deploy at 14:02 is the cause."}),
	}
}

func TestOneTurnIsOneStreamOpenedOnceAndClosedOnce(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	worker.answer(context.Background(), state.reply)

	calls := fake.made()
	if len(calls) == 0 {
		t.Fatal("the worker made no slack calls at all")
	}
	if calls[0] != "chat.startStream" {
		t.Errorf("the first call was %q, want the stream to be opened", calls[0])
	}
	if starts := count(calls, "chat.startStream"); starts != 1 {
		t.Errorf("the stream was opened %d times; a thread must hold one message per turn",
			starts)
	}
	if stops := count(calls, "chat.stopStream"); stops != 1 {
		t.Errorf("the stream was closed %d times, want once", stops)
	}
	if posts := count(calls, "chat.postMessage"); posts != 0 {
		t.Errorf("%d separate messages were posted; a streaming installation posts none",
			posts)
	}
	if !state.completed {
		t.Error("a delivered turn was not marked delivered, so it would be claimed again")
	}
}

func TestAConcludedSummaryIsTheFinalAnswer(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	worker.answer(context.Background(), state.reply)

	if appends := count(fake.made(), "chat.appendStream"); appends > 1 {
		t.Errorf("one batch produced %d appends; text must be coalesced", appends)
	}
	whole := strings.Join(fake.carried(), "")
	if !strings.Contains(whole, "The deploy at 14:02 is the cause.") {
		t.Errorf("the answer did not reach the thread whole: %q", whole)
	}
	if !strings.Contains(whole, "read 40 commits on checkout-api") {
		t.Errorf("the completed read is not in the thread: %q", whole)
	}
}

func TestAFinalAnswerLinksOnlyTheInvestigation(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	worker.PublicURL = "https://console.example.test/"
	worker.answer(context.Background(), state.reply)

	whole := strings.Join(fake.carried(), "")
	if !strings.Contains(whole, "/organizations/11111111-1111-4111-8111-111111111111/investigations/"+
		state.reply.Investigation.String()) {
		t.Errorf("the final answer has no stable Investigation link: %q", whole)
	}
	if strings.Contains(whole, "/sources") {
		t.Errorf("the final answer still links a retired Sources view: %q", whole)
	}
}

func TestAWorkerKilledMidStreamResumesRatherThanReposting(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn()[:3])
	worker.answer(context.Background(), state.reply)

	first := strings.Join(fake.carried(), "")
	state.mu.Lock()
	state.events = aTurn()
	resumed := state.reply
	state.mu.Unlock()

	worker.answer(context.Background(), resumed)

	if starts := count(fake.made(), "chat.startStream"); starts != 1 {
		t.Errorf("a resumed reply opened %d streams; it must continue the one that "+
			"exists, or the thread holds the answer twice", starts)
	}
	sent := fake.carried()
	second := strings.Join(sent[len(sent)-1:], "")
	if strings.Contains(second, "read 40 commits") && strings.Contains(first, "read 40 commits") {
		t.Errorf("the resumed pass repeated content already delivered: %q", second)
	}
}

func TestATransientFailureIsRetriedAndAddsNothingTwice(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	fake.failNext("chat.startStream", 1)
	worker, state := answering(t, fake, aTurn())

	worker.answer(context.Background(), state.reply)
	if len(fake.made()) != 0 {
		t.Fatalf("a failed open still made visible calls: %v", fake.made())
	}
	state.mu.Lock()
	if len(state.retries) != 1 {
		t.Errorf("a transient failure recorded %d retries, want one", len(state.retries))
	}
	retry := state.reply
	state.mu.Unlock()

	worker.answer(context.Background(), retry)
	if starts := count(fake.made(), "chat.startStream"); starts != 1 {
		t.Errorf("a retried reply opened %d streams, want one", starts)
	}
}

func TestWithoutStreamingOnePlaceholderIsEditedInPlace(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, false)
	worker, state := answering(t, fake, aTurn()[:3])
	worker.answer(context.Background(), state.reply)

	state.mu.Lock()
	state.events = aTurn()
	resumed := state.reply
	state.mu.Unlock()
	worker.answer(context.Background(), resumed)

	calls := fake.made()
	if posts := count(calls, "chat.postMessage"); posts != 1 {
		t.Errorf("%d messages were posted; reply must use ONE placeholder", posts)
	}
	if updates := count(calls, "chat.update"); updates == 0 {
		t.Errorf("the placeholder was never updated: %v", calls)
	}
	sent := fake.carried()
	last := sent[len(sent)-1]
	if !strings.Contains(last, "The deploy at 14:02 is the cause.") ||
		!strings.Contains(last, "read 40 commits") {
		t.Errorf("the placeholder's final text is not the whole answer: %q", last)
	}
}

func TestGivingUpIsRecordedAgainstTheDeliveryAndNotTheInvestigation(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	fake.failNext("chat.startStream", 100)
	worker, state := answering(t, fake, aTurn())

	state.mu.Lock()
	state.reply.Attempts = maxAttempts - 1
	attempt := state.reply
	state.mu.Unlock()

	worker.answer(context.Background(), attempt)

	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.gaveUp {
		t.Error("a reply past its attempt ceiling did not give up, so it retries forever")
	}
	if len(state.retries) == 0 || state.retries[0] == "" {
		t.Error("giving up recorded no reason an operator could read")
	}
	if state.completed {
		t.Error("a failed reply was marked delivered")
	}
}

func TestAFailedTurnSaysSoInTheThreadRatherThanGoingQuiet(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, []investigation.Event{
		progressed(1, investigation.EventStarted, nil),
		progressed(2, investigation.EventFailed,
			map[string]any{"reason": "no integration could be read"}),
	})
	worker.answer(context.Background(), state.reply)

	whole := strings.Join(fake.carried(), "")
	if !strings.Contains(whole, "no integration could be read") {
		t.Errorf("a failed turn said %q, which does not tell the thread anything", whole)
	}
}

func count(values []string, wanted string) int {
	found := 0
	for _, value := range values {
		if value == wanted {
			found++
		}
	}
	return found
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (d *repliesInMemory) UnnamedSlackAuthors(
	context.Context, uuid.UUID, uuid.UUID,
) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.unnamed...), nil
}

func (d *repliesInMemory) NameSlackAuthor(
	_ context.Context, _ uuid.UUID, _ uuid.UUID, actor, display string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.named == nil {
		d.named = map[string]string{}
	}
	d.named[actor] = display
	return nil
}

func TestAProcessThatDiesAfterOpeningTheMessageDoesNotRepost(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())

	fake.failNext("chat.appendStream", 100)
	worker.answer(context.Background(), state.reply)

	state.mu.Lock()
	held := state.reply
	state.mu.Unlock()
	if !held.Stream.Held() {
		t.Fatal("the message's identity was not recorded before content was sent, so a " +
			"crash here would repost")
	}
	for _, carried := range fake.carried() {
		if carried != "" {
			t.Errorf("opening the message carried content: %q", carried)
		}
	}

	fake.failNext("chat.appendStream", 0)
	worker.answer(context.Background(), held)
	if starts := count(fake.made(), "chat.startStream"); starts != 1 {
		t.Errorf("a resumed reply opened %d messages, want the one that exists", starts)
	}
}

func TestAReplyIntoAWorkspaceIsAuditedAsACollaborationWrite(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	worker.answer(context.Background(), state.reply)

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.audited) != 1 || state.audited[0] != "C0INCIDENTS" {
		t.Errorf("collaboration writes recorded = %v, want one naming the channel",
			state.audited)
	}
}

func TestTheThreadShowsTheWorkAsItHappens(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, false)
	worker, state := answering(t, fake, []investigation.Event{
		progressed(1, investigation.EventStarted, nil),
		progressed(2, investigation.EventToolStarted, map[string]any{"tool": "github.commits"}),
		progressed(3, investigation.EventToolCompleted,
			map[string]any{"summary": "read 40 commits on checkout-api"}),
	})
	worker.answer(context.Background(), state.reply)

	shown := strings.Join(fake.carried(), "\n")
	if !strings.Contains(shown, "read 40 commits on checkout-api") {
		t.Errorf("a completed read is not in the thread: %q", shown)
	}
	if !strings.Contains(shown, "Reading github.commits") {
		t.Errorf("what it is doing now is not in the thread: %q", shown)
	}
}

func TestTheAuthorsOfAThreadAreNamedRatherThanNumbered(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	fake.name("U9SRE", "priya")
	worker, state := answering(t, fake, aTurn())
	state.mu.Lock()
	state.reply.Conversation = uuid.New()
	state.unnamed = []string{"U9SRE"}
	claimedReply := state.reply
	state.mu.Unlock()

	worker.answer(context.Background(), claimedReply)

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.named["U9SRE"] != "priya" {
		t.Errorf("author names resolved to %v, want the workspace's own name for them",
			state.named)
	}
}

func TestAnUnresolvableNameDoesNotStopTheAnswer(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	state.mu.Lock()
	state.reply.Conversation = uuid.New()
	state.unnamed = []string{"U9GHOST"}
	claimedReply := state.reply
	state.mu.Unlock()

	worker.answer(context.Background(), claimedReply)

	if stops := count(fake.made(), "chat.stopStream"); stops != 1 {
		t.Errorf("an unresolvable author name stopped the answer: %v", fake.made())
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.named) != 0 {
		t.Errorf("a name that could not be resolved was recorded anyway: %v", state.named)
	}
}

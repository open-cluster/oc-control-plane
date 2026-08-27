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

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// ANSWERING IN A THREAD, ASSERTED ON THE SEQUENCE OF SLACK CALLS.
//
// What a person in the thread sees is the log of calls this worker made, so that is what is
// asserted: the stream is opened ONCE, appended to, and closed once. A retry appends what was
// missed rather than reposting what was seen. A worker killed mid-stream resumes. And where
// streaming is not available, one placeholder is edited in place and never a series of posts —
// which is the failure that makes a channel unreadable.

// slackCallLog is a fake Slack that records what it was asked to do.
type slackCallLog struct {
	*httptest.Server
	mu sync.Mutex
	// calls is every method asked for, in order. It IS the visible outcome.
	calls []string
	// text is what each call carried, so a repost is distinguishable from an append.
	text []string
	// streaming reports whether this workspace offers the native streaming methods.
	streaming bool
	// failFor makes the next n calls to a method fail transiently.
	failFor map[string]int
	// names are the display names this workspace will resolve. An id absent from it is an
	// account the token cannot see, which is a case the worker must survive.
	names map[string]string
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
				// The installation does not offer streaming. Answered as Slack would, so
				// the fallback is exercised by the same code path a real one takes.
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

// name teaches the fake what one user id is called.
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

// repliesInMemory is the durable state the worker reads and writes, in memory.
type repliesInMemory struct {
	mu     sync.Mutex
	reply  Reply
	events []investigation.Event
	// retries records every reschedule, which is what says a failure was recorded against
	// the DELIVERY.
	retries []string
	// audited is every channel a collaboration write was recorded against.
	audited []string
	// unnamed are the authors still recorded under a raw identifier, and named is what the
	// worker resolved them to.
	unnamed   []string
	named     map[string]string
	completed bool
	gaveUp    bool
	// sealed is the bot token as it rests. The worker opens it the way every other read
	// does, so the credential path is exercised rather than stubbed past.
	sealed []byte
}

func (d *repliesInMemory) ClaimSlackReplies(
	context.Context, int, time.Duration,
) ([]Reply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.completed || d.gaveUp {
		return nil, nil
	}
	return []Reply{d.reply}, nil
}

func (d *repliesInMemory) AdvanceSlackReply(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, made Progress,
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

// RecordCollaborationWrite counts the audit record the worker owes for putting a message in
// somebody's workspace. It is the only write this product makes into a system it does not own,
// so the test asserts it happens rather than trusting that it does.
func (d *repliesInMemory) RecordCollaborationWrite(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, where string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audited = append(d.audited, where)
	return nil
}

func (d *repliesInMemory) CompleteSlackReply(
	context.Context, tenancy.Organization, uuid.UUID,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completed = true
	return nil
}

func (d *repliesInMemory) RetrySlackReply(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID,
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
	context.Context, tenancy.Organization, uuid.UUID,
) (integrations.Integration, error) {
	return integrations.Integration{
		ID: d.reply.Integration, CredentialSealed: d.sealed,
	}, nil
}

func (d *repliesInMemory) Events(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, after int64, limit int,
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

// answering builds a worker over a fake Slack and an in-memory reply.
//
// The credential is really sealed and really opened, because a reply that could not open
// its token is a reply that answers nothing — and stubbing past that would leave the one
// failure most likely to happen in production untested.
func answering(t *testing.T, fake *slackCallLog, events []investigation.Event) (
	Worker, *repliesInMemory,
) {
	t.Helper()

	organization, err := tenancy.NewOrganization("org-a")
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
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
		progressed(3, investigation.EventAnswerDelta, map[string]any{"text": "The deploy at "}),
		progressed(4, investigation.EventAnswerDelta, map[string]any{"text": "14:02 is the cause."}),
		progressed(5, investigation.EventConcluded, map[string]any{"answer": ""}),
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

func TestAnAnswerIsCoalescedRatherThanSentPerToken(t *testing.T) {
	t.Parallel()

	// Two deltas arriving in one batch reach Slack as one call. A worker that called per
	// delta would be rate-limited into failure by its own enthusiasm.
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

func TestAFinalAnswerLinksTheInvestigationAndItsSources(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())
	worker.ConsoleURL = "https://console.example.test/"
	worker.answer(context.Background(), state.reply)

	whole := strings.Join(fake.carried(), "")
	if !strings.Contains(whole, "/organizations/org-a/investigations/"+
		state.reply.Investigation.String()) {
		t.Errorf("the final answer has no stable Investigation link: %q", whole)
	}
	if !strings.Contains(whole, "/organizations/org-a/investigations/"+
		state.reply.Investigation.String()+"/sources") {
		t.Errorf("the final answer has no Investigation Sources link: %q", whole)
	}
	if strings.Contains(whole, "/organizations/org-a/integrations/") {
		t.Errorf("the Sources link points to Integration setup instead of Investigation evidence: %q", whole)
	}
}

func TestAWorkerKilledMidStreamResumesRatherThanReposting(t *testing.T) {
	t.Parallel()

	// The first pass takes the first three events; the process then "dies" and a second
	// worker picks the reply up with the cursor where the first left it.
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
	// What the second pass sent must be what was MISSED, not what was already seen.
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

	// The first pass fails before anything visible exists.
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

	// The second pass succeeds and produces exactly one stream.
	worker.answer(context.Background(), retry)
	if starts := count(fake.made(), "chat.startStream"); starts != 1 {
		t.Errorf("a retried reply opened %d streams, want one", starts)
	}
}

func TestWithoutStreamingOnePlaceholderIsEditedInPlace(t *testing.T) {
	t.Parallel()

	// The failure this guards against is a series of separate posts, which is what makes a
	// channel unreadable and is the reason the stream design exists.
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
	// And the last edit carries the whole answer, because editing in place means sending
	// everything — an edit with only the new part would erase what came before.
	sent := fake.carried()
	last := sent[len(sent)-1]
	if !strings.Contains(last, "The deploy at 14:02 is the cause.") ||
		!strings.Contains(last, "read 40 commits") {
		t.Errorf("the placeholder's final text is not the whole answer: %q", last)
	}
}

func TestGivingUpIsRecordedAgainstTheDeliveryAndNotTheInvestigation(t *testing.T) {
	t.Parallel()

	// A Slack outage that never clears. The reply gives up; nothing here can touch the
	// investigation, which concludes on its own terms and stays complete in the console.
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

	// A question that got no answer and no explanation is the worst outcome available.
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

func TestNoModelReasoningCanReachAThread(t *testing.T) {
	t.Parallel()

	// The event stream carries no chain of thought by construction, and this is the
	// mechanical half of that promise: an event kind this build does not render puts
	// nothing in the thread.
	rendered := Render([]investigation.Event{
		progressed(1, investigation.EventCompacted,
			map[string]any{"text": "internal memory decision"}),
	})
	if rendered.Text != "" || len(rendered.Progress) != 0 {
		t.Errorf("an internal event rendered into the thread: %+v", rendered)
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

// testLogger discards, so a failing reply does not fill the test output with the log
// lines it is supposed to write.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The author names the worker resolved. A shared thread whose participants all read as U0…
// has attribution that technically survives and practically does not.
func (d *repliesInMemory) UnnamedSlackAuthors(
	context.Context, tenancy.Organization, uuid.UUID,
) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.unnamed...), nil
}

func (d *repliesInMemory) NameSlackAuthor(
	_ context.Context, _ tenancy.Organization, _ uuid.UUID, actor, display string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.named == nil {
		d.named = map[string]string{}
	}
	d.named[actor] = display
	return nil
}

// The window a crash could repost through, closed.
//
// The visible message is opened EMPTY and its identity recorded before any content is sent. A
// process that dies between the two resumes into the message it already opened; one that had
// posted content with the opening would have content outside the identity's protection, and the
// resumed pass would say it all again.
func TestAProcessThatDiesAfterOpeningTheMessageDoesNotRepost(t *testing.T) {
	t.Parallel()

	fake := newSlackCallLog(t, true)
	worker, state := answering(t, fake, aTurn())

	// The first pass opens the message and then everything after that fails, which stands
	// in for the process ending there.
	fake.failNext("chat.appendStream", 100)
	worker.answer(context.Background(), state.reply)

	state.mu.Lock()
	held := state.reply
	state.mu.Unlock()
	if !held.Stream.Held() {
		t.Fatal("the message's identity was not recorded before content was sent, so a " +
			"crash here would repost")
	}
	// Nothing visible was said yet: opening it carried no content.
	for _, carried := range fake.carried() {
		if carried != "" {
			t.Errorf("opening the message carried content: %q", carried)
		}
	}

	// The resumed pass opens nothing new and says everything once.
	fake.failNext("chat.appendStream", 0)
	worker.answer(context.Background(), held)
	if starts := count(fake.made(), "chat.startStream"); starts != 1 {
		t.Errorf("a resumed reply opened %d messages, want the one that exists", starts)
	}
}

// Putting a message in somebody else's workspace is on the record.
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

// The plan and the reads as they happen, not only the answer at the end.
func TestTheThreadShowsTheWorkAsItHappens(t *testing.T) {
	t.Parallel()

	// A turn that has read something and is still working. What a person watching wants is
	// evidence it is doing something, and one completed read with what it found is that.
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

// A shared thread stays readable: the people in it are named, not numbered.
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

// A name that cannot be resolved leaves the identity in place, which still attributes the
// message. The answer matters more than the label on the question.
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

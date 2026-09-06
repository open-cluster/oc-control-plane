package slack

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// Delivery has its own cursor and outcome. Slack acceptance and local persistence
// are separate: interruption between them can repeat a message or native append.

// The flush boundary. Whichever comes first: enough text to be worth a call, or enough time
// that a reader would think nothing was happening.
const (
	flushBytes    = 400
	flushInterval = 900 * time.Millisecond
)

// Retry shape. Bounded exponential backoff with jitter; the ceiling exists because a reply
// that has failed this many times is not going to succeed by waiting longer, and its thread
// deserves to be told rather than left silent.
const (
	retryBase     = 2 * time.Second
	retryCeiling  = 2 * time.Minute
	maxAttempts   = 8
	leaseDuration = 2 * time.Minute
)

// Reply is one investigation's answer on its way into one thread.
type Reply struct {
	Investigation  uuid.UUID
	ClaimToken     uuid.UUID
	LeaseExpiresAt time.Time
	Organization   tenancy.Organization
	Integration    uuid.UUID
	// Conversation is the thread's conversation. The reply is written into the thread and
	// the conversation is what holds who said what in it.
	Conversation uuid.UUID
	// Stream is the visible message and how it is written to. Its TS is empty until the
	// first successful call, and its presence is what tells a resumed reply to continue
	// rather than start.
	Stream Stream
	// LastSequence is the cursor. Everything at or below it is already in the thread.
	LastSequence int64
	Attempts     int
}

// Progress is what one pass established. It travels as one value because the two halves are
// recorded together and always have been — passed apart they are two positional arguments of
// the same shape, which is a swap nothing would catch.
type Progress struct {
	Stream Stream
	// Sequence is the highest event now rendered into the thread. It only moves forward.
	Sequence int64
}

var ErrReplyClaimLost = errors.New("slack reply claim lost")

// Replies is what the worker needs from durable state. It is declared here because the
// collaboration delivery owns its vocabulary and persistence depends on it.
type Replies interface {
	// ClaimSlackReplies assigns unique ownership to each due reply.
	ClaimSlackReplies(ctx context.Context, limit int, lease time.Duration) ([]Reply, error)
	// AdvanceSlackReply records what one pass established: the visible message's identity
	// once it exists, and how far the cursor has moved. It only ever moves forward.
	AdvanceSlackReply(ctx context.Context, org tenancy.Organization, investigation, owner uuid.UUID,
		made Progress) error
	// CompleteSlackReply marks one answered. Nothing claims it again.
	CompleteSlackReply(ctx context.Context, org tenancy.Organization,
		investigation, owner uuid.UUID) error
	// RetrySlackReply schedules another attempt, or gives up when there is no attempt left
	// worth making. The note is this build's own words.
	RetrySlackReply(ctx context.Context, org tenancy.Organization, investigation, owner uuid.UUID,
		at time.Time, note string, giveUp bool) error
	ReleaseSlackReply(ctx context.Context, org tenancy.Organization, investigation, owner uuid.UUID, at time.Time) error
	// RecordCollaborationWrite puts one reply into a customer's workspace on the audit
	// record. It is the only thing this product writes into a system it does not own, and
	// "what did OpenCluster say in our Slack" has to be answerable.
	RecordCollaborationWrite(ctx context.Context, org tenancy.Organization,
		integration uuid.UUID, where string) error
	// Integration reads the installation a reply answers through, for its credential.
	Integration(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (integrations.Integration, error)
	// UnnamedSlackAuthors reports the Slack identities in one conversation still recorded
	// under their raw identifier, and NameSlackAuthor records what one is called.
	//
	// They are here rather than on the endpoint that accepts a message because resolving a
	// name costs a call to the vendor, and the acknowledgement path is the one place that
	// must make none: Slack retries anything it is not answered inside three seconds.
	UnnamedSlackAuthors(ctx context.Context, org tenancy.Organization,
		conversation uuid.UUID) ([]string, error)
	NameSlackAuthor(ctx context.Context, org tenancy.Organization, conversation uuid.UUID,
		actor, display string) error
	// Events reports an investigation's events after a sequence, in order, bounded.
	Events(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		after int64, limit int) ([]investigation.Event, error)
}

// Worker answers investigations in their Slack threads.
type Worker struct {
	Replies Replies
	Client  *Client
	Sealer  seal.Sealer
	Logger  *slog.Logger
	// Counters records what happened to each attempt, and may be its zero value.
	Counters Instruments
	// ConsoleURL is the trusted deployment origin used for terminal navigation links.
	ConsoleURL string
	// Interval is how often the worker looks when it found nothing to do. A pass that DID
	// work looks again after the flush interval instead, which is what bounds how often
	// one streaming turn calls Slack.
	Interval time.Duration
	// Batch bounds how many replies one pass takes.
	Batch int
}

// Run works until the context ends.
func (w Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Second
	}
	batch := w.Batch
	if batch <= 0 {
		batch = 8
	}

	for {
		worked := w.pass(ctx, batch)
		if ctx.Err() != nil {
			return
		}
		wait := interval
		if worked {
			wait = flushInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// pass takes one batch and reports whether anything moved.
func (w Worker) pass(ctx context.Context, batch int) bool {
	worked := false
	for range batch {
		claimed, err := w.Replies.ClaimSlackReplies(ctx, 1, leaseDuration)
		if err != nil {
			if ctx.Err() == nil {
				w.Logger.ErrorContext(ctx, "claiming slack replies failed",
					slog.String("error", err.Error()))
			}
			return worked
		}
		if len(claimed) == 0 {
			return worked
		}
		reply := claimed[0]
		if ctx.Err() != nil {
			return worked
		}
		attempt, cancel := context.WithDeadline(ctx, reply.LeaseExpiresAt.Add(-time.Second))
		if w.answer(attempt, reply) {
			worked = true
		}
		if errors.Is(attempt.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			settlement, stop := context.WithDeadline(ctx, reply.LeaseExpiresAt)
			w.retry(settlement, reply, "slack delivery exceeded its attempt deadline")
			stop()
		}
		cancel()
	}
	return worked
}

// answer advances one reply as far as it can, and reports whether it moved.
func (w Worker) answer(ctx context.Context, reply Reply) bool {
	token, err := w.credential(ctx, reply)
	if err != nil {
		w.retry(ctx, reply, "this integration's credential could not be opened")
		return false
	}

	events, err := w.Replies.Events(ctx, reply.Organization, reply.Investigation,
		reply.LastSequence, 200)
	if err != nil {
		w.retry(ctx, reply, "the investigation's events could not be read")
		return false
	}
	if len(events) == 0 {
		if reply.LastSequence > 0 && reply.Stream.Held() {
			last, readErr := w.Replies.Events(ctx, reply.Organization, reply.Investigation, reply.LastSequence-1, 1)
			if readErr != nil {
				w.retry(ctx, reply, "the investigation's final event could not be read")
				return false
			}
			if len(last) == 1 && last[0].Sequence == reply.LastSequence && Render(last).Done {
				w.finish(ctx, token, reply)
				return true
			}
		}
		w.release(ctx, reply)
		return false
	}

	rendered := Render(events)
	if rendered.Done {
		rendered.Footer = w.navigation(ctx, reply)
	}
	if held(rendered) {
		// The size half of the flush boundary. A few words with nothing else to say are
		// held for the next pass rather than spent on a call: Slack is never called per
		// token, and a turn that is still streaming will have more in a moment.
		//
		// Never held once the turn is done, and never when there is progress to show —
		// those are the two things a person watching the thread is waiting to see.
		w.release(ctx, reply)
		return false
	}

	if !reply.Stream.Held() {
		// Save identity before content. Provider acceptance before this save remains
		// an at-least-once boundary; a database token cannot deduplicate Slack writes.
		stream, startErr := w.Client.StartStream(ctx, token,
			reply.Stream.Channel, reply.Stream.Thread)
		if startErr != nil {
			w.retry(ctx, reply, "slack would not open the reply")
			return false
		}
		reply.Stream = stream
		if err := w.Replies.AdvanceSlackReply(ctx, reply.Organization, reply.Investigation, reply.ClaimToken,
			Progress{Stream: stream, Sequence: reply.LastSequence}); err != nil {
			w.Logger.ErrorContext(ctx, "recording a slack reply's message failed",
				slog.String("error", err.Error()))
			return false
		}
		w.audit(ctx, reply)
		// Who asked, in the names the people in the thread use for each other. Done once,
		// here, because this is the first moment the credential is in hand under no
		// deadline anybody is watching — and because a shared thread whose participants
		// all read as U0… has attribution that technically survives and practically does
		// not.
		w.name(ctx, token, reply)
	}

	if err := w.send(ctx, token, reply, rendered); err != nil {
		w.retry(ctx, reply, "slack would not take the reply")
		return false
	}
	if err := w.Replies.AdvanceSlackReply(ctx, reply.Organization, reply.Investigation, reply.ClaimToken,
		Progress{Stream: reply.Stream, Sequence: events[len(events)-1].Sequence}); err != nil {
		w.Logger.ErrorContext(ctx, "recording slack reply progress failed",
			slog.String("error", err.Error()))
		return false
	}

	if rendered.Done {
		w.finish(ctx, token, reply)
	} else {
		w.release(ctx, reply)
	}
	return true
}

func (w Worker) finish(ctx context.Context, token string, reply Reply) {
	if err := w.Client.StopStream(ctx, token, reply.Stream); err != nil {
		w.retry(ctx, reply, "slack would not close the reply")
		return
	}
	if err := w.Replies.CompleteSlackReply(ctx, reply.Organization, reply.Investigation, reply.ClaimToken); err != nil {
		w.Logger.ErrorContext(ctx, "completing a slack reply failed", slog.String("error", err.Error()))
		return
	}
	w.Counters.countReply(ctx, replyAnswered)
}

func (w Worker) release(ctx context.Context, reply Reply) {
	if err := w.Replies.ReleaseSlackReply(ctx, reply.Organization, reply.Investigation, reply.ClaimToken, time.Now().Add(flushInterval)); err != nil && ctx.Err() == nil {
		w.Logger.ErrorContext(ctx, "releasing a slack reply failed", slog.String("error", err.Error()))
	}
}

// send writes this batch into the visible message.
//
// The two shapes are genuinely different writes and must not be confused: a native stream is
// APPENDED to, and a placeholder is REPLACED with everything rendered so far. Appending to a
// placeholder would erase the answer; replacing a stream would repeat it.
func (w Worker) send(
	ctx context.Context, token string, reply Reply, rendered Rendered,
) error {
	if reply.Stream.Native {
		return w.Client.AppendStream(ctx, token, reply.Stream, visible(rendered, false))
	}

	// Everything from the beginning, because editing in place means sending the whole text.
	// Bounded by one turn's events, and read again rather than held in a column: the event
	// stream is already the durable record of what this turn said, and a second copy of it
	// would be a second thing to keep correct.
	all, err := w.Replies.Events(ctx, reply.Organization, reply.Investigation, 0, 500)
	if err != nil {
		return err
	}
	allRendered := Render(all)
	allRendered.Footer = rendered.Footer
	return w.Client.ReplaceStream(ctx, token, reply.Stream, visible(allRendered, true))
}

// held reports a batch too small to be worth a call on its own.
func held(rendered Rendered) bool {
	return !rendered.Done && len(rendered.Progress) == 0 && len(rendered.Text) < flushBytes
}

// visible composes what a person reads: the task updates that have happened, then the answer.
//
// withStatus is true only for the shape that REPLACES its whole text. A transient status line
// can be replaced; appended to a native stream it would accumulate, and a thread collecting
// every "Reading…" would be the transcript this design exists to avoid.
func visible(rendered Rendered, withStatus bool) string {
	var text strings.Builder
	for _, line := range rendered.Progress {
		text.WriteString("• " + line + "\n")
	}
	if withStatus && !rendered.Done && rendered.Status != "" {
		text.WriteString("_" + rendered.Status + "…_\n")
	}
	if rendered.Text != "" {
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(rendered.Text)
	}
	if rendered.Failed && rendered.Text == "" {
		text.WriteString("\n" + FailureNotice)
	}
	if rendered.Footer != "" {
		text.WriteString("\n\n" + rendered.Footer)
	}
	return text.String()
}

func (w Worker) navigation(_ context.Context, reply Reply) string {
	base := strings.TrimSuffix(w.ConsoleURL, "/")
	if base == "" {
		return ""
	}
	organization := url.PathEscape(reply.Organization.String())
	investigationURL := base + "/organizations/" + organization + "/investigations/" +
		reply.Investigation.String()
	return "<" + investigationURL + "|Open Investigation>"
}

// credential opens the bot token this reply answers with.
func (w Worker) credential(ctx context.Context, reply Reply) (string, error) {
	integration, err := w.Replies.Integration(ctx, reply.Organization, reply.Integration)
	if err != nil {
		return "", err
	}
	if integration.Disabled() {
		// An operator turned it off. Reading stops and answering stops.
		return "", errors.New("slack: this integration is disabled")
	}
	if len(integration.CredentialSealed) == 0 {
		return "", errors.New("slack: this integration holds no credential")
	}
	return w.Sealer.Open(integration.CredentialSealed,
		integrations.CredentialBinding(integration.ID))
}

// name resolves the Slack identities in this conversation that are still recorded under their
// raw identifier.
//
// Best effort throughout. A name that cannot be resolved — a token without users:read, a
// deactivated account, a vendor that is refusing — leaves the identifier in place, which still
// attributes the message and is the honest rendering of "we could not find out". Nothing here
// may fail the reply: the answer matters more than the label on the question.
func (w Worker) name(ctx context.Context, token string, reply Reply) {
	if reply.Conversation == uuid.Nil {
		return
	}
	unnamed, err := w.Replies.UnnamedSlackAuthors(ctx, reply.Organization, reply.Conversation)
	if err != nil {
		w.Logger.WarnContext(ctx, "reading a conversation's unnamed authors failed",
			slog.String("error", err.Error()))
		return
	}
	for _, actor := range unnamed {
		display := w.Client.UserName(ctx, token, actor)
		if display == "" || display == actor {
			continue
		}
		if err := w.Replies.NameSlackAuthor(ctx, reply.Organization, reply.Conversation,
			actor, display); err != nil {
			w.Logger.WarnContext(ctx, "naming a slack author failed",
				slog.String("error", err.Error()))
			return
		}
	}
}

// audit records the collaboration write, once per turn: the moment OpenCluster's message
// appears in a workspace it does not own.
func (w Worker) audit(ctx context.Context, reply Reply) {
	if err := w.Replies.RecordCollaborationWrite(ctx, reply.Organization, reply.Integration,
		reply.Stream.Channel); err != nil {
		w.Logger.ErrorContext(ctx, "recording a collaboration write failed",
			slog.String("error", err.Error()))
	}
}

// retry schedules another attempt, or gives up and says so.
//
// The note is this build's own words and never the vendor's. An operator reads it, and a
// message a far end chose is text somebody else chose.
func (w Worker) retry(ctx context.Context, reply Reply, note string) {
	giveUp := reply.Attempts+1 >= maxAttempts
	at := time.Now().Add(backoff(reply.Attempts))
	if err := w.Replies.RetrySlackReply(ctx, reply.Organization, reply.Investigation, reply.ClaimToken,
		at, note, giveUp); err != nil {
		w.Logger.ErrorContext(ctx, "rescheduling a slack reply failed",
			slog.String("error", err.Error()))
		return
	}
	outcome, level := replyRetried, slog.LevelWarn
	if giveUp {
		outcome, level = replyAbandoned, slog.LevelError
	}
	w.Counters.countReply(ctx, outcome)
	w.Logger.Log(ctx, level, "a slack reply did not complete",
		slog.String("org_id", reply.Organization.String()),
		slog.String("investigation_id", reply.Investigation.String()),
		slog.Int("attempts", reply.Attempts+1),
		slog.Bool("gave_up", giveUp),
		slog.String("note", note))
}

// backoff is bounded exponential with jitter. The jitter matters: a Slack outage fails every
// reply at once, and without it they would all come back at once too.
func backoff(attempts int) time.Duration {
	wait := min(retryBase<<min(attempts, 6), retryCeiling)
	return wait + time.Duration(rand.Int64N(int64(wait/2)+1))
}

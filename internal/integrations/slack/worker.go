package slack

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ANSWERING BACK IN THE THREAD.
//
// One streamed message per turn, driven from the investigation's PERSISTED event stream by a
// cursor of its own. That is what makes every hard case ordinary: a transient failure retries
// and appends what was missed rather than reposting what was seen, and a worker killed
// mid-stream resumes from the cursor instead of starting the answer again.
//
// A DELIVERY FAILURE IS NEVER AN INVESTIGATION FAILURE. They are related concerns and not the
// same one. The investigation concludes or fails on its own terms and its record stays
// complete and readable in the console whatever Slack did — a chat outage must not be able to
// cancel or corrupt the work behind it.
//
// SLACK IS NEVER CALLED PER TOKEN. Text is coalesced and flushed on a size-or-interval
// boundary chosen to sit well inside the published rate tier. A worker that called Slack for
// every delta would be rate-limited into failure by its own enthusiasm.

// The flush boundary. Whichever comes first: enough text to be worth a call, or enough time
// that a reader would think nothing was happening.
const (
	flushBytes    = 400
	flushInterval = 900 * time.Millisecond
)

// Retry shape. Bounded exponential backoff with jitter; the ceiling exists because a delivery
// that has failed this many times is not going to succeed by waiting longer, and its thread
// deserves to be told rather than left silent.
const (
	retryBase     = 2 * time.Second
	retryCeiling  = 2 * time.Minute
	maxAttempts   = 8
	leaseDuration = 2 * time.Minute
)

// Delivery is one investigation's answer on its way into one thread.
type Delivery struct {
	Investigation uuid.UUID
	Organization  tenancy.Organization
	Integration   uuid.UUID
	Channel       string
	Thread        string
	// StreamTS names the visible message, empty until the first successful call. Its
	// presence is what tells a resumed delivery to continue rather than start.
	StreamTS string
	// Streaming records which of the two shapes this reply is. A resumed delivery must not
	// guess: appending to a placeholder would erase it.
	Streaming bool
	// LastSequence is the cursor. Everything at or below it is already in the thread.
	LastSequence int64
	Attempts     int
}

// Deliveries is what the worker needs from durable state. It is declared here because the
// capability owns its vocabulary and persistence depends on it.
type Deliveries interface {
	// ClaimSlackDeliveries leases the deliveries that are due, so two workers cannot write
	// into one visible message.
	ClaimSlackDeliveries(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error)
	// AdvanceSlackDelivery records progress: the visible message's identity once it
	// exists, and how far the cursor has moved. It only ever moves forward.
	AdvanceSlackDelivery(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		streamTS string, streaming bool, sequence int64) error
	// CompleteSlackDelivery marks one delivered. Nothing claims it again.
	CompleteSlackDelivery(ctx context.Context, org tenancy.Organization,
		investigation uuid.UUID) error
	// RetrySlackDelivery schedules another attempt, or gives up when there is no attempt
	// left worth making. The note is this build's own words.
	RetrySlackDelivery(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		at time.Time, note string, giveUp bool) error
	// Integration reads the installation a delivery answers through, for its credential.
	Integration(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (integrations.Integration, error)
	// Events reports an investigation's events after a sequence, in order, bounded.
	Events(ctx context.Context, org tenancy.Organization, investigation uuid.UUID,
		after int64, limit int) ([]investigation.Event, error)
}

// Worker delivers investigations into their Slack threads.
type Worker struct {
	Deliveries Deliveries
	Client     *Client
	Sealer     seal.Sealer
	Logger     *slog.Logger
	// Interval is how often the worker looks for due deliveries. It is a floor rather than
	// a cadence: a pass that found work looks again immediately, because a turn that is
	// streaming has more to say now rather than in a second.
	Interval time.Duration
	// Batch bounds how many deliveries one pass takes.
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
			// Something is streaming. Looking again immediately is the difference between
			// an answer that appears and one that arrives a second at a time.
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// pass takes one batch and reports whether anything was delivered.
func (w Worker) pass(ctx context.Context, batch int) bool {
	claimed, err := w.Deliveries.ClaimSlackDeliveries(ctx, batch, leaseDuration)
	if err != nil {
		if ctx.Err() == nil {
			w.Logger.ErrorContext(ctx, "claiming slack deliveries failed",
				slog.String("error", err.Error()))
		}
		return false
	}

	worked := false
	for _, delivery := range claimed {
		if ctx.Err() != nil {
			return worked
		}
		if w.deliver(ctx, delivery) {
			worked = true
		}
	}
	return worked
}

// deliver advances one delivery as far as it can, and reports whether it moved.
func (w Worker) deliver(ctx context.Context, delivery Delivery) bool {
	token, err := w.credential(ctx, delivery)
	if err != nil {
		w.retry(ctx, delivery, "this integration's credential could not be opened")
		return false
	}

	events, err := w.Deliveries.Events(ctx, delivery.Organization, delivery.Investigation,
		delivery.LastSequence, 200)
	if err != nil {
		w.retry(ctx, delivery, "the investigation's events could not be read")
		return false
	}
	if len(events) == 0 {
		return false
	}

	rendered := Render(events)
	reply := Reply{
		Channel: delivery.Channel, Thread: delivery.Thread,
		TS: delivery.StreamTS, Streaming: delivery.Streaming,
	}

	if reply.TS == "" {
		// The turn's one visible message, opened once. Its identity is recorded BEFORE
		// anything else is sent, so a worker that dies immediately afterwards resumes into
		// the message it already posted rather than posting a second one.
		reply, err = w.Client.StartReply(ctx, token, delivery.Channel, delivery.Thread,
			opening(rendered))
		if err != nil {
			w.retry(ctx, delivery, "slack would not open the reply")
			return false
		}
		if err := w.Deliveries.AdvanceSlackDelivery(ctx, delivery.Organization,
			delivery.Investigation, reply.TS, reply.Streaming,
			events[len(events)-1].Sequence); err != nil {
			w.Logger.ErrorContext(ctx, "recording slack delivery progress failed",
				slog.String("error", err.Error()))
			return false
		}
	} else if err := w.send(ctx, token, reply, delivery, rendered); err != nil {
		w.retry(ctx, delivery, "slack would not take the reply")
		return false
	} else if err := w.Deliveries.AdvanceSlackDelivery(ctx, delivery.Organization,
		delivery.Investigation, reply.TS, reply.Streaming,
		events[len(events)-1].Sequence); err != nil {
		w.Logger.ErrorContext(ctx, "recording slack delivery progress failed",
			slog.String("error", err.Error()))
		return false
	}

	if rendered.Done {
		if err := w.Client.StopReply(ctx, token, reply); err != nil {
			// The content is delivered and only the close failed. Retrying would risk
			// nothing being added and is worth one more attempt, but the answer is already
			// visible either way.
			w.retry(ctx, delivery, "slack would not close the reply")
			return true
		}
		if err := w.Deliveries.CompleteSlackDelivery(ctx, delivery.Organization,
			delivery.Investigation); err != nil {
			w.Logger.ErrorContext(ctx, "completing a slack delivery failed",
				slog.String("error", err.Error()))
		}
	}
	return true
}

// send writes this batch into the visible message.
//
// The two shapes are genuinely different writes and must not be confused: a stream is
// APPENDED to, and a placeholder is REPLACED with everything rendered so far. Appending to a
// placeholder would erase the answer; replacing a stream would repeat it.
func (w Worker) send(
	ctx context.Context, token string, reply Reply, delivery Delivery, rendered Rendered,
) error {
	if reply.Streaming {
		return w.Client.AppendReply(ctx, token, reply, visible(rendered))
	}

	// Everything from the beginning, because editing in place means sending the whole text.
	// Bounded by one turn's events, and read again rather than held in a column: the event
	// stream is already the durable record of what this turn said, and a second copy of it
	// would be a second thing to keep correct.
	all, err := w.Deliveries.Events(ctx, delivery.Organization, delivery.Investigation, 0, 500)
	if err != nil {
		return err
	}
	return w.Client.ReplaceReply(ctx, token, reply, visible(Render(all)))
}

// opening is what the message says when it is first posted.
func opening(rendered Rendered) string {
	if text := visible(rendered); text != "" {
		return text
	}
	return ""
}

// visible composes what a person reads: the progress lines that have completed, then the
// answer. The status line is deliberately not repeated into the text — it is momentary, and a
// thread that accumulated every "Reading…" would be the transcript this design exists to
// avoid.
func visible(rendered Rendered) string {
	var text strings.Builder
	for _, line := range rendered.Progress {
		text.WriteString("• " + line + "\n")
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
	return text.String()
}

// credential opens the bot token this delivery answers with.
func (w Worker) credential(ctx context.Context, delivery Delivery) (string, error) {
	integration, err := w.Deliveries.Integration(ctx, delivery.Organization, delivery.Integration)
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

// retry schedules another attempt, or gives up and says so.
//
// The note is this build's own words and never the vendor's. An operator reads it, and a
// message a far end chose is text somebody else chose.
func (w Worker) retry(ctx context.Context, delivery Delivery, note string) {
	giveUp := delivery.Attempts+1 >= maxAttempts
	at := time.Now().Add(backoff(delivery.Attempts))
	if err := w.Deliveries.RetrySlackDelivery(ctx, delivery.Organization,
		delivery.Investigation, at, note, giveUp); err != nil {
		w.Logger.ErrorContext(ctx, "rescheduling a slack delivery failed",
			slog.String("error", err.Error()))
		return
	}
	level := slog.LevelWarn
	if giveUp {
		level = slog.LevelError
	}
	w.Logger.Log(ctx, level, "a slack delivery did not complete",
		slog.String("org_id", delivery.Organization.String()),
		slog.String("investigation_id", delivery.Investigation.String()),
		slog.Int("attempts", delivery.Attempts+1),
		slog.Bool("gave_up", giveUp),
		slog.String("note", note))
}

// backoff is bounded exponential with jitter. The jitter matters: a Slack outage fails every
// delivery at once, and without it they would all come back at once too.
func backoff(attempts int) time.Duration {
	wait := retryBase << min(attempts, 6)
	if wait > retryCeiling {
		wait = retryCeiling
	}
	return wait + time.Duration(rand.Int64N(int64(wait/2)+1))
}

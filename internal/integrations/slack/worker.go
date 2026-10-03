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
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

const (
	flushBytes    = 400
	flushInterval = 900 * time.Millisecond
)

const (
	retryBase     = 2 * time.Second
	retryCeiling  = 2 * time.Minute
	maxAttempts   = 8
	leaseDuration = 2 * time.Minute
)

type Reply struct {
	Investigation  uuid.UUID
	ClaimToken     uuid.UUID
	LeaseExpiresAt time.Time
	Organization   uuid.UUID
	Integration    uuid.UUID
	Conversation   uuid.UUID
	Stream         Stream
	LastSequence   int64
	Attempts       int
}

type Progress struct {
	Stream   Stream
	Sequence int64
}

var ErrReplyClaimLost = errors.New("slack reply claim lost")

type Replies interface {
	ClaimSlackReplies(ctx context.Context, limit int, lease time.Duration) ([]Reply, error)
	AdvanceSlackReply(ctx context.Context, org uuid.UUID, investigation, owner uuid.UUID,
		made Progress) error
	CompleteSlackReply(ctx context.Context, org uuid.UUID,
		investigation, owner uuid.UUID) error
	RetrySlackReply(ctx context.Context, org uuid.UUID, investigation, owner uuid.UUID,
		at time.Time, note string, giveUp bool) error
	ReleaseSlackReply(ctx context.Context, org uuid.UUID, investigation, owner uuid.UUID, at time.Time) error
	RecordCollaborationWrite(ctx context.Context, org uuid.UUID,
		integration uuid.UUID, where string) error
	Integration(ctx context.Context, org uuid.UUID,
		id uuid.UUID) (integrations.Integration, error)
	UnnamedSlackAuthors(ctx context.Context, org uuid.UUID,
		conversation uuid.UUID) ([]string, error)
	NameSlackAuthor(ctx context.Context, org uuid.UUID, conversation uuid.UUID,
		actor, display string) error
	Events(ctx context.Context, org uuid.UUID, investigation uuid.UUID,
		after int64, limit int) ([]investigation.Event, error)
}

type Worker struct {
	Replies   Replies
	Client    *Client
	Sealer    seal.Sealer
	Logger    *slog.Logger
	Counters  Instruments
	PublicURL string
	Interval  time.Duration
	Batch     int
}

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
		w.release(ctx, reply)
		return false
	}

	if !reply.Stream.Held() {
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

func (w Worker) send(
	ctx context.Context, token string, reply Reply, rendered Rendered,
) error {
	if reply.Stream.Native {
		return w.Client.AppendStream(ctx, token, reply.Stream, visible(rendered, false))
	}

	all, err := w.Replies.Events(ctx, reply.Organization, reply.Investigation, 0, 500)
	if err != nil {
		return err
	}
	allRendered := Render(all)
	allRendered.Footer = rendered.Footer
	return w.Client.ReplaceStream(ctx, token, reply.Stream, visible(allRendered, true))
}

func held(rendered Rendered) bool {
	return !rendered.Done && len(rendered.Progress) == 0 && len(rendered.Text) < flushBytes
}

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
	base := strings.TrimSuffix(w.PublicURL, "/")
	if base == "" {
		return ""
	}
	organization := url.PathEscape(reply.Organization.String())
	investigationURL := base + "/organizations/" + organization + "/investigations/" +
		reply.Investigation.String()
	return "<" + investigationURL + "|Open Investigation>"
}

func (w Worker) credential(ctx context.Context, reply Reply) (string, error) {
	integration, err := w.Replies.Integration(ctx, reply.Organization, reply.Integration)
	if err != nil {
		return "", err
	}
	if integration.Disabled {
		return "", errors.New("slack: this integration is disabled")
	}
	if len(integration.CredentialSealed) == 0 {
		return "", errors.New("slack: this integration holds no credential")
	}
	return w.Sealer.Open(integration.CredentialSealed,
		integrations.CredentialBinding(integration.ID))
}

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

func (w Worker) audit(ctx context.Context, reply Reply) {
	if err := w.Replies.RecordCollaborationWrite(ctx, reply.Organization, reply.Integration,
		reply.Stream.Channel); err != nil {
		w.Logger.ErrorContext(ctx, "recording a collaboration write failed",
			slog.String("error", err.Error()))
	}
}

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

func backoff(attempts int) time.Duration {
	wait := min(retryBase<<min(attempts, 6), retryCeiling)
	return wait + time.Duration(rand.Int64N(int64(wait/2)+1))
}

package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// JobHandler hides one provider's domain effect. The durable worker owns only leasing,
// retry, and dispatch; it never switches over provider payloads or workflows.
type JobHandler interface {
	Handle(context.Context, storage.WebhookJob) error
}

type JobHandlers map[storage.WebhookJobKind]JobHandler

type Worker struct {
	Jobs        *storage.Database
	Handlers    JobHandlers
	Owner       string
	Lease       time.Duration
	RetryBase   time.Duration
	MaxAttempts int
	Logger      *slog.Logger
	Counters    JobInstruments
}

func (w Worker) Run(ctx context.Context) {
	interval := 250 * time.Millisecond
	for {
		worked, err := w.ProcessOne(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger().ErrorContext(ctx, "webhook job processing failed", slog.String("error", err.Error()))
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (w Worker) ProcessOne(ctx context.Context) (bool, error) {
	lease := w.Lease
	if lease <= 0 {
		lease = time.Minute
	}
	job, found, err := w.Jobs.ClaimWebhookJob(ctx, w.Owner, lease)
	if err != nil || !found {
		return found, err
	}
	w.Counters.ObserveDelay(ctx, job.UpdatedAt.Sub(job.CreatedAt))
	if job.Attempts >= storage.MaxWebhookJobAttempts {
		return true, w.fail(ctx, job, errors.New("the accepted webhook delivery exhausted its processing budget"))
	}
	handler := w.Handlers[job.Kind]
	if handler == nil {
		return true, w.fail(ctx, job, errors.New("this build has no handler for the webhook job kind"))
	}
	if err := w.handleWithLease(ctx, job, handler, lease); err != nil {
		if errors.Is(err, storage.ErrWebhookJobLeaseLost) {
			return true, nil
		}
		if errors.Is(err, storage.ErrWebhookJobCapacity) {
			w.Counters.Count(ctx, "delayed")
			return true, w.Jobs.DeferWebhookJob(ctx, job.Organization, job,
				time.Second)
		}
		return true, w.fail(ctx, job, err)
	}
	return true, nil
}

func (w Worker) handleWithLease(
	ctx context.Context, job storage.WebhookJob, handler JobHandler, lease time.Duration,
) error {
	handlerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(max(lease/3, 10*time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-handlerContext.Done():
				return
			case <-ticker.C:
				if err := w.Jobs.HeartbeatWebhookJob(handlerContext, job.Organization, job, lease); err != nil {
					lost <- err
					cancel()
					return
				}
			}
		}
	}()
	err := handler.Handle(handlerContext, job)
	cancel()
	<-stopped
	if err == nil {
		return nil
	}
	select {
	case renewalError := <-lost:
		if errors.Is(renewalError, storage.ErrWebhookJobLeaseLost) {
			return storage.ErrWebhookJobLeaseLost
		}
		return renewalError
	default:
		return err
	}
}

func (w Worker) fail(ctx context.Context, job storage.WebhookJob, cause error) error {
	maximum := w.MaxAttempts
	if maximum <= 0 {
		maximum = 8
	}
	base := w.RetryBase
	if base <= 0 {
		base = time.Second
	}
	terminal := job.Attempts >= maximum
	delay := base << min(job.Attempts-1, 8)
	class := "provider-job-failed"
	message := "the accepted webhook delivery could not be processed"
	if err := w.Jobs.FailWebhookJob(ctx, job.Organization, job, terminal, delay,
		class, message); err != nil && !errors.Is(err, storage.ErrWebhookJobLeaseLost) {
		return err
	}
	if terminal {
		w.Counters.Count(ctx, "failed")
	} else {
		w.Counters.Count(ctx, "delayed")
	}
	w.logger().WarnContext(ctx, message, slog.String("delivery_id", job.DeliveryID.String()),
		slog.String("failure_class", class), slog.Int("attempt", job.Attempts),
		slog.String("cause", cause.Error()))
	return nil
}

func (w Worker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

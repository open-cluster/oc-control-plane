package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// WorkHandler hides one provider's domain effect. The durable worker owns only leasing,
// retry, and dispatch; it never switches over provider payloads or workflows.
type WorkHandler interface {
	Handle(context.Context, storage.WebhookWork) error
}

type WorkHandlers map[storage.WebhookWorkKind]WorkHandler

type Worker struct {
	Work        *storage.Database
	Handlers    WorkHandlers
	Owner       string
	Lease       time.Duration
	RetryBase   time.Duration
	MaxAttempts int
	Logger      *slog.Logger
	Counters    WorkInstruments
}

func (w Worker) Run(ctx context.Context) {
	interval := 250 * time.Millisecond
	for {
		worked, err := w.ProcessOne(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger().ErrorContext(ctx, "webhook delivery processing failed", slog.String("error", err.Error()))
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
	work, found, err := w.Work.ClaimWebhookWork(ctx, w.Owner, lease)
	if err != nil || !found {
		return found, err
	}
	w.Counters.ObserveDelay(ctx, work.UpdatedAt.Sub(work.CreatedAt))
	if work.Attempts >= storage.MaxWebhookWorkAttempts {
		return true, w.fail(ctx, work, errors.New("the accepted webhook delivery exhausted its processing budget"))
	}
	handler := w.Handlers[work.Kind]
	if handler == nil {
		return true, w.fail(ctx, work, errors.New("this build has no handler for the work kind"))
	}
	if err := w.handleWithLease(ctx, work, handler, lease); err != nil {
		if errors.Is(err, storage.ErrWebhookWorkLeaseLost) {
			return true, nil
		}
		if errors.Is(err, storage.ErrWebhookWorkCapacity) {
			w.Counters.Count(ctx, "delayed")
			return true, w.Work.DeferWebhookWork(ctx, work.Organization, work,
				time.Second)
		}
		return true, w.fail(ctx, work, err)
	}
	return true, nil
}

func (w Worker) handleWithLease(
	ctx context.Context, work storage.WebhookWork, handler WorkHandler, lease time.Duration,
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
				if err := w.Work.HeartbeatWebhookWork(handlerContext, work.Organization, work, lease); err != nil {
					lost <- err
					cancel()
					return
				}
			}
		}
	}()
	err := handler.Handle(handlerContext, work)
	cancel()
	<-stopped
	if err == nil {
		return nil
	}
	select {
	case renewalError := <-lost:
		if errors.Is(renewalError, storage.ErrWebhookWorkLeaseLost) {
			return storage.ErrWebhookWorkLeaseLost
		}
		return renewalError
	default:
		return err
	}
}

func (w Worker) fail(ctx context.Context, work storage.WebhookWork, cause error) error {
	maximum := w.MaxAttempts
	if maximum <= 0 {
		maximum = 8
	}
	base := w.RetryBase
	if base <= 0 {
		base = time.Second
	}
	terminal := work.Attempts >= maximum
	delay := base << min(work.Attempts-1, 8)
	class := "provider-work-failed"
	message := "the accepted webhook delivery could not be processed"
	if err := w.Work.FailWebhookWork(ctx, work.Organization, work, terminal, delay,
		class, message); err != nil && !errors.Is(err, storage.ErrWebhookWorkLeaseLost) {
		return err
	}
	if terminal {
		w.Counters.Count(ctx, "failed")
	} else {
		w.Counters.Count(ctx, "delayed")
	}
	w.logger().WarnContext(ctx, message, slog.String("delivery_id", work.DeliveryID.String()),
		slog.String("failure_class", class), slog.Int("attempt", work.Attempts),
		slog.String("cause", cause.Error()))
	return nil
}

func (w Worker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

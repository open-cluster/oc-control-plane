package audit

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Forwarder is the edition boundary for delivering the authoritative application event.
// Recorded.ID is the idempotency identity at every remote sink.
type Forwarder interface {
	Forward(context.Context, Recorded) error
}

// ForwardingDelivery is one leased outbox item.
type ForwardingDelivery struct {
	Event    Recorded
	Attempts int
}

// ForwardingOutbox is the durable queue contract implemented by PostgreSQL.
type ForwardingOutbox interface {
	ClaimAuditDeliveries(context.Context, string, time.Time, time.Time, int) ([]ForwardingDelivery, error)
	CompleteAuditDelivery(context.Context, string, string) error
	FailAuditDelivery(context.Context, string, string, time.Time, bool) error
}

// ForwardingWorker asynchronously drains committed events to an edition forwarder.
type ForwardingWorker struct {
	Outbox      ForwardingOutbox
	Forwarder   Forwarder
	Owner       string
	Lease       time.Duration
	RetryBase   time.Duration
	MaxAttempts int
	Batch       int
	Logger      *slog.Logger
}

// Run drains on a fixed cadence until shutdown. A sweep is attempted immediately so a
// restarted instance does not add one whole interval to an existing backlog.
func (w ForwardingWorker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	w.deliverAndReport(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.deliverAndReport(ctx, now)
		}
	}
}

func (w ForwardingWorker) deliverAndReport(ctx context.Context, at ...time.Time) {
	now := time.Now().UTC()
	if len(at) > 0 {
		now = at[0]
	}
	if _, err := w.DeliverReady(ctx, now); err != nil && w.Logger != nil && ctx.Err() == nil {
		w.Logger.Error("audit forwarding sweep failed")
	}
}

// DeliverReady performs one bounded sweep. Remote failures are durable retry state, not a
// sweep failure; database failures are returned because the lease state is then unknown.
func (w ForwardingWorker) DeliverReady(ctx context.Context, now time.Time) (int, error) {
	if w.Outbox == nil || w.Forwarder == nil {
		return 0, errors.New("audit forwarding requires an outbox and forwarder")
	}
	if w.Owner == "" || w.Lease <= 0 || w.RetryBase <= 0 || w.MaxAttempts <= 0 || w.Batch <= 0 {
		return 0, errors.New("audit forwarding worker has invalid bounds")
	}
	deliveries, err := w.Outbox.ClaimAuditDeliveries(ctx, w.Owner, now, now.Add(w.Lease), w.Batch)
	if err != nil {
		return 0, err
	}
	for _, delivery := range deliveries {
		if forwardErr := w.Forwarder.Forward(ctx, delivery.Event); forwardErr != nil {
			attempt := delivery.Attempts + 1
			terminal := attempt >= w.MaxAttempts
			next := now.Add(w.retryDelay(attempt))
			if err = w.Outbox.FailAuditDelivery(ctx, w.Owner, delivery.Event.ID, next, terminal); err != nil {
				return len(deliveries), err
			}
			if w.Logger != nil {
				w.Logger.Warn("audit forwarding failed",
					slog.String("event_id", delivery.Event.ID),
					slog.Int("attempt", attempt), slog.Bool("terminal", terminal))
			}
			continue
		}
		if err = w.Outbox.CompleteAuditDelivery(ctx, w.Owner, delivery.Event.ID); err != nil {
			return len(deliveries), err
		}
	}
	return len(deliveries), nil
}

func (w ForwardingWorker) retryDelay(attempt int) time.Duration {
	shift := attempt - 1
	if shift > 8 {
		shift = 8
	}
	return w.RetryBase * time.Duration(1<<shift)
}

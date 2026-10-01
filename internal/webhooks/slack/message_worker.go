package slack

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type MessageWorker struct {
	Database        *storage.Database
	References      *SlackReferenceResolver
	WindowLead      time.Duration
	MaxWaitingTurns int
	Owner           string
	Lease           time.Duration
	RetryBase       time.Duration
	MaxAttempts     int
	Logger          *slog.Logger
	Counters        MessageInstruments
}

func (w MessageWorker) Run(ctx context.Context) {
	for {
		worked, err := w.ProcessOne(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger().ErrorContext(ctx, "slack message processing failed", slog.String("error", err.Error()))
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (w MessageWorker) ProcessOne(ctx context.Context) (bool, error) {
	lease := w.Lease
	if lease <= 0 {
		lease = time.Minute
	}
	work, found, err := w.Database.ClaimSlackMessageWork(ctx, w.Owner, lease)
	if err != nil || !found {
		return found, err
	}
	w.Counters.ObserveDelay(ctx, work.UpdatedAt.Sub(work.CreatedAt))
	if work.Attempts >= storage.MaxSlackMessageAttempts {
		return true, w.fail(ctx, work, errors.New("the accepted Slack message exhausted its processing budget"))
	}
	if err := w.processWithLease(ctx, work, lease); err != nil {
		if errors.Is(err, storage.ErrSlackMessageLeaseLost) {
			return true, nil
		}
		if errors.Is(err, storage.ErrInvestigationCapacity) {
			w.Counters.Count(ctx, "delayed")
			return true, w.Database.DeferSlackMessageWork(ctx, work.Organization, work, time.Second)
		}
		return true, w.fail(ctx, work, err)
	}
	return true, nil
}

func (w MessageWorker) processWithLease(ctx context.Context, work storage.SlackMessageWork, lease time.Duration) error {
	processing, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(max(lease/3, 10*time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-processing.Done():
				return
			case <-ticker.C:
				if err := w.Database.HeartbeatSlackMessageWork(processing, work.Organization, work, lease); err != nil {
					lost <- err
					cancel()
					return
				}
			}
		}
	}()
	err := w.process(processing, work)
	cancel()
	<-stopped
	if err == nil {
		return nil
	}
	select {
	case renewalError := <-lost:
		if errors.Is(renewalError, storage.ErrSlackMessageLeaseLost) {
			return storage.ErrSlackMessageLeaseLost
		}
		return renewalError
	default:
		return err
	}
}

func (w MessageWorker) process(ctx context.Context, work storage.SlackMessageWork) error {
	if w.References != nil {
		lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := w.References.Resolve(lookup, work)
		cancel()
		if err != nil {
			w.logger().WarnContext(ctx, "slack message provenance lookup failed",
				slog.String("delivery_id", work.DeliveryID.String()))
		}
	}
	return w.Database.ApplySlackMessageWork(ctx, work.Organization, work, w.WindowLead, w.MaxWaitingTurns)
}

func (w MessageWorker) fail(ctx context.Context, work storage.SlackMessageWork, cause error) error {
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
	class := "provider-job-failed"
	message := "the accepted webhook delivery could not be processed"
	if err := w.Database.FailSlackMessageWork(ctx, work.Organization, work, terminal, delay,
		class, message); err != nil && !errors.Is(err, storage.ErrSlackMessageLeaseLost) {
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

func (w MessageWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

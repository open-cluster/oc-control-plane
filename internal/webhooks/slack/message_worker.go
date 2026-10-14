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
		worked, _ := w.ProcessOne(ctx)
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

func (w MessageWorker) ProcessOne(ctx context.Context) (worked bool, err error) {
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger().ErrorContext(ctx, "Slack Message processing could not commit", slog.String("error", "storage operation failed"))
		}
	}()
	lease := w.Lease
	if lease <= 0 {
		lease = time.Minute
	}
	work, found, err := w.Database.ClaimSlackMessageWork(ctx, w.Owner, lease)
	if err != nil || !found {
		return found, err
	}
	w.Logger = w.logger().With(slog.String("org_id", work.Organization.String()),
		slog.String("integration_id", work.IntegrationID.String()), slog.String("delivery_id", work.DeliveryID.String()))
	w.Counters.ObserveDelay(ctx, work.UpdatedAt.Sub(work.CreatedAt))
	if work.Attempts >= storage.MaxSlackMessageAttempts {
		return true, w.fail(ctx, work, errors.New("the accepted Slack message exhausted its processing budget"))
	}
	if err := w.processWithLease(ctx, work, lease); err != nil {
		if errors.Is(err, storage.ErrSlackMessageLeaseLost) {
			return true, nil
		}
		if errors.Is(err, storage.ErrInvestigationCapacity) {
			if err := w.Database.DeferSlackMessageWork(ctx, work.Organization, work, time.Second); err != nil {
				if errors.Is(err, storage.ErrSlackMessageLeaseLost) {
					return true, nil
				}
				return true, err
			}
			w.Counters.Count(ctx, "delayed")
			return true, nil
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
				slog.String("error", "source reference could not be resolved"))
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
		class, message); err != nil {
		if errors.Is(err, storage.ErrSlackMessageLeaseLost) {
			return nil
		}
		return err
	}
	level := slog.LevelWarn
	if terminal {
		w.Counters.Count(ctx, "failed")
		level = slog.LevelError
	} else {
		w.Counters.Count(ctx, "delayed")
	}
	failure := "Slack Message processing failed"
	if errors.Is(cause, context.DeadlineExceeded) {
		failure = "Slack Message processing timed out"
	}
	w.logger().Log(ctx, level, message,
		slog.String("failure_class", class), slog.Int("attempt", work.Attempts),
		slog.Bool("terminal", terminal), slog.String("error", failure))
	return nil
}

func (w MessageWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

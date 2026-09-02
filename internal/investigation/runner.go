package investigation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	investigationTimeout = 30 * time.Minute
)

type RunnerStore interface {
	ClaimInvestigation(context.Context, Claim) (tenancy.Organization, Investigation, bool, error)
	Heartbeat(context.Context, tenancy.Organization, uuid.UUID, Claim) (bool, error)
	RecoverStale(context.Context, string, int) (int, error)
	Investigation(context.Context, tenancy.Organization, uuid.UUID) (Investigation, error)
	DrainConversation(context.Context, tenancy.Organization, uuid.UUID, time.Duration, int) (bool, error)
	DrainQueuedConversation(context.Context, time.Duration, int) (bool, error)
}

// Runner owns only queued-work lifecycle and lease coordination.
type Runner struct {
	Store      RunnerStore
	Agent      Agent
	Workers    int
	MaxPending int
	Worker     string
	WindowLead time.Duration
	Logger     *slog.Logger
	Telemetry  *Telemetry

	identity sync.Once
	worker   string
	mu       sync.Mutex
	stops    map[uuid.UUID]context.CancelFunc
}

func (r *Runner) Run(ctx context.Context) {
	workers := r.Workers
	if workers <= 0 {
		workers = 8
	}
	var running sync.WaitGroup
	running.Add(workers + 2)
	for range workers {
		go func() {
			defer running.Done()
			r.workerLoop(ctx)
		}()
	}
	go func() {
		defer running.Done()
		r.recoverLoop(ctx)
	}()
	go func() {
		defer running.Done()
		r.drainLoop(ctx)
	}()
	<-ctx.Done()
	r.cancelAll()
	running.Wait()
}

func (r *Runner) Cancel(id uuid.UUID) {
	r.mu.Lock()
	stop := r.stops[id]
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (r *Runner) workerLoop(ctx context.Context) {
	for ctx.Err() == nil {
		organization, opened, claimed, err := r.Store.ClaimInvestigation(ctx, r.claim())
		if err != nil {
			r.logError(ctx, "investigations could not be claimed", err)
		}
		if err != nil || !claimed {
			r.wait(ctx, claimInterval)
			continue
		}
		r.runClaimed(ctx, organization, opened)
	}
}

func (r *Runner) runClaimed(
	ctx context.Context, organization tenancy.Organization, opened Investigation,
) {
	r.Telemetry.claimed(opened.CreatedAt)
	runCtx, stop := context.WithTimeout(ctx, investigationTimeout)
	r.track(opened.ID, stop)
	done := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		r.watch(runCtx, stop, done, organization, opened.ID)
	}()

	err := r.Agent.Run(runCtx, organization, opened)
	close(done)
	stop()
	<-watchDone
	r.untrack(opened.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.logError(ctx, "an investigation could not be terminalized", err)
		}
		found, readErr := r.Store.Investigation(ctx, organization, opened.ID)
		if readErr != nil || found.Status == StatusRunning {
			return
		}
	}
	r.drain(ctx, organization, opened)
}

func (r *Runner) watch(
	ctx context.Context, stop context.CancelFunc, done <-chan struct{},
	organization tenancy.Organization, id uuid.UUID,
) {
	renew := time.NewTicker(heartbeatInterval)
	cancelled := time.NewTicker(time.Second)
	defer renew.Stop()
	defer cancelled.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-cancelled.C:
			found, err := r.Store.Investigation(ctx, organization, id)
			if err == nil && found.Status == StatusCancelled {
				stop()
				return
			}
		case <-renew.C:
			held, err := r.Store.Heartbeat(ctx, organization, id, r.claim())
			if err != nil {
				r.logError(ctx, "an investigation lease could not be renewed", err)
				continue
			}
			if !held {
				stop()
				return
			}
		}
	}
}

func (r *Runner) recoverLoop(ctx context.Context) {
	for r.wait(ctx, sweepInterval) {
		recovered, err := r.Store.RecoverStale(ctx, RecoveryReason, sweepBatch)
		if err != nil {
			r.logError(ctx, "stale investigations could not be recovered", err)
			continue
		}
		if recovered > 0 {
			r.Telemetry.RecoveredStale(recovered)
			r.Logger.Warn("stale investigations were recovered", slog.Int("recovered", recovered))
		}
	}
}

func (r *Runner) drainLoop(ctx context.Context) {
	for r.wait(ctx, claimInterval) {
		if _, err := r.Store.DrainQueuedConversation(ctx, r.WindowLead, r.MaxPending); err != nil {
			r.logError(ctx, "queued conversation work could not be admitted", err)
		}
	}
}

func (r *Runner) drain(
	ctx context.Context, organization tenancy.Organization, finished Investigation,
) {
	if finished.ConversationID == uuid.Nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.Store.DrainConversation(writeCtx, organization,
		finished.ConversationID, r.WindowLead, r.MaxPending); err != nil {
		r.logError(writeCtx, "a conversation's queued messages could not be taken up", err)
	}
}

func (r *Runner) claim() Claim {
	return Claim{Worker: r.workerName(), LeaseFor: leaseDuration}
}

func (r *Runner) workerName() string {
	r.identity.Do(func() {
		r.worker = r.Worker
		if r.worker == "" {
			host, err := os.Hostname()
			if err != nil || host == "" {
				host = "worker"
			}
			r.worker = fmt.Sprintf("%s/%d", host, os.Getpid())
		}
	})
	return r.worker
}

func (r *Runner) track(id uuid.UUID, stop context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stops == nil {
		r.stops = make(map[uuid.UUID]context.CancelFunc)
	}
	r.stops[id] = stop
}

func (r *Runner) untrack(id uuid.UUID) {
	r.mu.Lock()
	delete(r.stops, id)
	r.mu.Unlock()
}

func (r *Runner) cancelAll() {
	r.mu.Lock()
	stops := make([]context.CancelFunc, 0, len(r.stops))
	for _, stop := range r.stops {
		stops = append(stops, stop)
	}
	r.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

func (r *Runner) wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Runner) logError(ctx context.Context, message string, err error) {
	if ctx.Err() == nil && r.Logger != nil {
		r.Logger.ErrorContext(ctx, message, slog.String("error", err.Error()))
	}
}

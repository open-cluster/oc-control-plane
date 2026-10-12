package investigation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	investigationTimeout = 30 * time.Minute
)

type RunnerStore interface {
	ClaimInvestigation(context.Context, Claim) (uuid.UUID, Investigation, bool, error)
	Heartbeat(context.Context, uuid.UUID, uuid.UUID, Claim) (bool, error)
	RecoverStale(context.Context, string, int) (int, error)
	Investigation(context.Context, uuid.UUID, uuid.UUID) (Investigation, error)
	OpenNextConversationTurn(context.Context, uuid.UUID, uuid.UUID, time.Duration, int) (bool, error)
	OpenQueuedConversationTurn(context.Context, time.Duration, int) (bool, error)
}

type Runner struct {
	Store      RunnerStore
	Agent      Agent
	Workers    int
	MaxPending int
	Worker     string
	WindowLead time.Duration
	Logger     *slog.Logger
	Telemetry  *Telemetry

	identity       sync.Once
	worker         string
	mu             sync.Mutex
	runningCancels map[uuid.UUID]context.CancelFunc
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
		r.admitQueuedConversations(ctx)
	}()
	<-ctx.Done()
	r.cancelRunningInvestigations()
	running.Wait()
}

func (r *Runner) Cancel(id uuid.UUID) {
	r.mu.Lock()
	cancelRun := r.runningCancels[id]
	r.mu.Unlock()
	if cancelRun != nil {
		cancelRun()
	}
}

func (r *Runner) workerLoop(ctx context.Context) {
	for ctx.Err() == nil {
		organization, investigation, claimed, err := r.Store.ClaimInvestigation(ctx, r.workerClaim())
		if err != nil {
			r.logError(ctx, "investigations could not be claimed", err)
		}
		if err != nil || !claimed {
			r.wait(ctx, claimInterval)
			continue
		}
		r.runClaimed(ctx, organization, investigation)
	}
}

func (r *Runner) runClaimed(
	ctx context.Context, organization uuid.UUID, investigation Investigation,
) {
	r.Telemetry.claimed(investigation.CreatedAt)
	runCtx, cancelRun := context.WithTimeout(ctx, investigationTimeout)
	r.trackRunning(investigation.ID, cancelRun)
	agentDone := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		claim := r.workerClaim()
		claim.Token = investigation.ClaimToken
		r.monitorClaim(runCtx, cancelRun, agentDone, organization, investigation.ID, claim)
	}()

	err := r.Agent.Run(runCtx, organization, investigation)
	close(agentDone)
	cancelRun()
	<-monitorDone
	r.untrackRunning(investigation.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.logError(ctx, "an investigation could not be terminalized", err)
		}
		found, readErr := r.Store.Investigation(ctx, organization, investigation.ID)
		if readErr != nil || found.Status == StatusRunning {
			return
		}
	}
	r.openNextConversationTurn(ctx, organization, investigation)
}

// monitorClaim keeps the lease alive and stops local execution when the
// Investigation is cancelled or the claim is lost.
func (r *Runner) monitorClaim(
	ctx context.Context, cancelRun context.CancelFunc, agentDone <-chan struct{},
	organization uuid.UUID, id uuid.UUID, claim Claim,
) {
	renew := time.NewTicker(heartbeatInterval)
	cancelled := time.NewTicker(time.Second)
	defer renew.Stop()
	defer cancelled.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-agentDone:
			return
		case <-cancelled.C:
			found, err := r.Store.Investigation(ctx, organization, id)
			if err == nil && found.Status == StatusCancelled {
				cancelRun()
				return
			}
		case <-renew.C:
			held, err := r.Store.Heartbeat(ctx, organization, id, claim)
			if err != nil {
				r.logError(ctx, "an investigation lease could not be renewed", err)
				continue
			}
			if !held {
				cancelRun()
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

func (r *Runner) admitQueuedConversations(ctx context.Context) {
	for r.wait(ctx, claimInterval) {
		if _, err := r.Store.OpenQueuedConversationTurn(ctx, r.WindowLead, r.MaxPending); err != nil {
			r.logError(ctx, "queued conversation work could not be admitted", err)
		}
	}
}

// openNextConversationTurn admits queued Messages after the current Investigation ends.
func (r *Runner) openNextConversationTurn(
	ctx context.Context, organization uuid.UUID, finished Investigation,
) {
	if finished.ConversationID == uuid.Nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.Store.OpenNextConversationTurn(writeCtx, organization,
		finished.ConversationID, r.WindowLead, r.MaxPending); err != nil {
		r.logError(writeCtx, "a conversation's queued messages could not be taken up", err)
	}
}

func (r *Runner) workerClaim() Claim {
	return Claim{
		Worker:   r.workerName(),
		LeaseFor: leaseDuration,
	}
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

// trackRunning makes a running Investigation cancellable by this process.
func (r *Runner) trackRunning(id uuid.UUID, cancelRun context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runningCancels == nil {
		r.runningCancels = make(map[uuid.UUID]context.CancelFunc)
	}
	r.runningCancels[id] = cancelRun
}

// untrackRunning removes a finished Investigation from the local cancellation registry.
func (r *Runner) untrackRunning(id uuid.UUID) {
	r.mu.Lock()
	delete(r.runningCancels, id)
	r.mu.Unlock()
}

func (r *Runner) cancelRunningInvestigations() {
	r.mu.Lock()
	runningCancels := make([]context.CancelFunc, 0, len(r.runningCancels))
	for _, cancelRun := range r.runningCancels {
		runningCancels = append(runningCancels, cancelRun)
	}
	r.mu.Unlock()
	for _, cancelRun := range runningCancels {
		cancelRun()
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

const (
	heartbeatInterval = 2 * time.Minute
	leaseDuration     = 15 * time.Minute
	claimInterval     = 500 * time.Millisecond
	sweepInterval     = time.Minute
	sweepBatch        = 50
)

const RecoveryReason = "worker interrupted"

type Claim struct {
	Worker   string
	LeaseFor time.Duration
	Token    uuid.UUID
}

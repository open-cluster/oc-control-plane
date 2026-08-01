package investigation

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The background worker: what makes an investigation survive a deploy.
//
// A round is opened by the request that asked for it and claimed by a worker afterwards. That gap
// is the point — the run is durable before anything starts executing it, so a control-plane restart
// between the two returns the round to claimable rather than losing it, and a worker that dies
// mid-round loses its lease and the work comes back.

// Pending discovers which tenants have work waiting.
//
// It is deliberately not part of Store. Every method there takes an organization because an ambient
// tenant is how one customer is served another's rows; this one has no organization to take, because
// its whole job is to find out which organizations there are work for. It reads no tenant data —
// only which tenants have a claimable round — and that is why it is safe.
type Pending interface {
	InvestigationsAwaitingWork(ctx context.Context) ([]tenancy.Organization, error)
}

// Worker claims rounds and runs them.
type Worker struct {
	Runner  Runner
	Store   Store
	Pending Pending
	Logger  *slog.Logger
	// Interval is how often the worker looks for work. It is a poll rather than a notification
	// because the alternative is a second delivery mechanism beside the database, and the database
	// is already the thing that decides who owns a round.
	Interval time.Duration
	// LeaseFor is how long a claim holds a round before it is claimable again. It is renewed while
	// the round runs; the value only decides how long a round waits after the worker holding it
	// dies.
	LeaseFor time.Duration
	// Capacity is the most rounds this worker holds at once.
	Capacity int
	// SessionID identifies this worker's claims. A new one per process is what makes a restart's
	// leases distinguishable from the new instance's.
	SessionID uuid.UUID
}

// Run claims and executes rounds until the context is cancelled, then waits for what it holds.
//
// Cancellation does not abandon a round mid-write: each running round sees the cancelled context,
// stops at its next step, and records the outcome it reached. What it cannot finish stays leased
// until the lease runs out and another instance takes it, which is the recoverable half of the
// same mechanism.
func (w Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	var running sync.WaitGroup
	defer running.Wait()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		w.sweep(ctx, &running)
	}
}

// sweep claims what is waiting, once, and starts it.
func (w Worker) sweep(ctx context.Context, running *sync.WaitGroup) {
	organizations, err := w.Pending.InvestigationsAwaitingWork(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.Logger.ErrorContext(ctx, "looking for investigation work failed",
				slog.String("error", err.Error()))
		}
		return
	}

	for _, organization := range organizations {
		claimed, claimErr := w.Store.ClaimRounds(ctx, organization, RoundClaim{
			SessionID: w.SessionID,
			LeaseFor:  w.LeaseFor,
			Capacity:  w.Capacity,
		})
		if claimErr != nil {
			if ctx.Err() == nil {
				w.Logger.ErrorContext(ctx, "claiming investigation rounds failed",
					slog.String("organization", organization.String()),
					slog.String("error", claimErr.Error()))
			}
			continue
		}
		for _, held := range claimed {
			running.Add(1)
			go func() {
				defer running.Done()
				w.execute(ctx, organization, held)
			}()
		}
	}
}

// execute runs one round with its lease kept alive underneath it.
func (w Worker) execute(ctx context.Context, organization tenancy.Organization, held Claimed) {
	beating, stop := context.WithCancel(ctx)
	defer stop()
	go w.heartbeat(beating, organization, held.Fence())

	w.Logger.InfoContext(ctx, "investigation round claimed",
		slog.String("organization", organization.String()),
		slog.String("investigation_id", held.Investigation.ID.String()),
		slog.String("round_id", held.Round.ID.String()),
		slog.Int("ordinal", held.Round.Ordinal))

	if err := w.Runner.Run(ctx, organization, held); err != nil {
		w.Logger.ErrorContext(ctx, "an investigation round could not be ended",
			slog.String("organization", organization.String()),
			slog.String("round_id", held.Round.ID.String()),
			slog.String("error", err.Error()))
	}
}

// heartbeat renews the lease while the round runs, so a round that legitimately takes longer than
// one lease is not taken away from the worker executing it.
//
// It stops at the first refusal. A refused renewal means the round is no longer this worker's —
// cancelled, or claimed by another instance — and renewing harder would not change that.
func (w Worker) heartbeat(ctx context.Context, organization tenancy.Organization, fence Fence) {
	// Renewed well inside the lease, so one missed tick does not lose the round.
	interval := w.LeaseFor / 3
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := w.Store.RenewRoundLease(ctx, organization, fence, w.LeaseFor); err != nil {
			if !errors.Is(err, ErrLeaseLost) && ctx.Err() == nil {
				w.Logger.WarnContext(ctx, "renewing an investigation round lease failed",
					slog.String("round_id", fence.RoundID.String()),
					slog.String("error", err.Error()))
			}
			return
		}
	}
}

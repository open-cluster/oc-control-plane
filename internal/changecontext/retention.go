package changeledger

import (
	"context"
	"log/slog"
	"time"
)

// Bounds on one sweep, for the same reason the audit pruner has them: a quarter of a
// busy fleet's change history is not a DELETE anybody should hold locks for.
const (
	pruneBatch         = 1000
	maxBatchesPerSweep = 50
)

// Retention is what the pruner needs from durable state. Declared here because the
// capability owns its vocabulary and persistence depends on it; a test's implementation
// needs no database.
type Retention interface {
	// PruneChangeLedgerBefore removes at most limit entries older than the horizon,
	// across every database, reporting how many went.
	PruneChangeLedgerBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Pruner applies the ledger's retention on an interval. Purely by age and deliberately
// simpler than the audit pruner's per-tenant schedule: the ledger is derived operational
// context, its retention is the deployment's, and a pruned entry is recoverable as a
// fresh baseline the next time a Relay observes the object.
type Pruner struct {
	Retention Retention
	Logger    *slog.Logger
	// Days is how long an entry is kept.
	Days int
	// Interval is how often the horizon is applied. Retention is measured in days, so
	// hourly is close enough to be honest and far enough to cost nothing.
	Interval time.Duration
	// Now is the clock, injectable so a test can age rows without waiting a day out.
	Now func() time.Time
}

// Run applies the horizon until the context ends. The first sweep waits for the first
// tick, so a crash-looping process does not scan per restart.
func (p Pruner) Run(ctx context.Context) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Sweep(ctx)
		}
	}
}

// Sweep works the backlog down in bounded batches, stopping early when a batch comes
// back short — that is the backlog gone — and at a fixed batch count either way, so a
// horizon applied for the first time against months of history spreads over several
// sweeps instead of one long-held lock.
func (p Pruner) Sweep(ctx context.Context) {
	horizon := p.now().AddDate(0, 0, -p.Days).UTC()
	var removed int64
	for range maxBatchesPerSweep {
		if ctx.Err() != nil {
			return
		}
		batch, err := p.Retention.PruneChangeLedgerBefore(ctx, horizon, pruneBatch)
		removed += batch
		if err != nil {
			p.Logger.ErrorContext(ctx, "the change ledger's retention could not be applied",
				slog.String("error", err.Error()))
			return
		}
		if batch < pruneBatch {
			break
		}
	}
	if removed > 0 {
		p.Logger.InfoContext(ctx, "change ledger entries removed by retention",
			slog.Int("retention_days", p.Days),
			slog.Int64("removed", removed))
	}
}

func (p Pruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

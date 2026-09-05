package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	pruneBatch         = 1000
	maxBatchesPerSweep = 50
)

// Retention is one tenant's declared schedule.
type Retention struct {
	Organization tenancy.Organization
	// Days is how long the tenant says it keeps the record. It is always positive here: zero
	// declares no schedule, which means the product's default of keeping everything.
	Days int
}

// Horizon is the instant before which this tenant's events have aged out.
func (r Retention) Horizon(now time.Time) time.Time {
	return now.AddDate(0, 0, -r.Days).UTC()
}

type Retentions interface {
	// DeclaredRetentions reports every tenant that has declared a schedule, across every
	// database this deployment serves.
	DeclaredRetentions(ctx context.Context) ([]Retention, error)
	// PruneEventsBefore removes at most limit events older than the horizon, reporting how many went.
	PruneEventsBefore(ctx context.Context, organization tenancy.Organization,
		before time.Time, limit int) (int64, error)
}

type Pruner struct {
	Retentions Retentions
	Logger     *slog.Logger
	Interval   time.Duration
	Now        func() time.Time
}

// Run applies the schedule until the context is cancelled.
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

func (p Pruner) Sweep(ctx context.Context) {
	declared, err := p.Retentions.DeclaredRetentions(ctx)
	if err != nil {
		p.Logger.ErrorContext(ctx, "the declared audit retention schedules could not be read",
			slog.String("error", err.Error()))
		return
	}

	for _, retention := range declared {
		removed, pruneErr := p.prune(ctx, retention)
		if pruneErr != nil {
			p.Logger.ErrorContext(ctx, "a tenant's audit retention schedule could not be applied",
				slog.String("organization", retention.Organization.String()),
				slog.Int("retention_days", retention.Days),
				slog.String("error", pruneErr.Error()))
			continue
		}
		if removed == 0 {
			continue
		}
		p.Logger.InfoContext(ctx, "audit events removed by the tenant's retention schedule",
			slog.String("organization", retention.Organization.String()),
			slog.Int("retention_days", retention.Days),
			slog.Int64("removed", removed))
	}
}

func (p Pruner) prune(ctx context.Context, retention Retention) (int64, error) {
	horizon := retention.Horizon(p.now())

	var removed int64
	for range maxBatchesPerSweep {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		batch, err := p.Retentions.PruneEventsBefore(
			ctx, retention.Organization, horizon, pruneBatch)
		removed += batch
		if err != nil {
			return removed, err
		}
		if batch < pruneBatch {
			return removed, nil
		}
	}
	// The backlog outlasted this sweep. It is stated rather than left to be inferred from a
	// figure that happens to be a multiple of the batch size.
	p.Logger.InfoContext(ctx, "a tenant's audit backlog outlasted this sweep and continues next",
		slog.String("organization", retention.Organization.String()),
		slog.Int64("removed", removed))
	return removed, nil
}

func (p Pruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

package changes

import (
	"context"
	"log/slog"
	"time"
)

const (
	pruneBatch         = 1000
	maxBatchesPerSweep = 50
)

type Retention interface {
	PruneChangesBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}

type Pruner struct {
	Retention Retention
	Logger    *slog.Logger
	Days      int
	Interval  time.Duration
	Now       func() time.Time
}

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
	horizon := p.now().AddDate(0, 0, -p.Days).UTC()
	var removed int64
	for range maxBatchesPerSweep {
		if ctx.Err() != nil {
			return
		}
		batch, err := p.Retention.PruneChangesBefore(ctx, horizon, pruneBatch)
		removed += batch
		if err != nil {
			p.Logger.ErrorContext(ctx, "change retention could not be applied",
				slog.String("error", err.Error()))
			return
		}
		if batch < pruneBatch {
			break
		}
	}
	if removed > 0 {
		p.Logger.InfoContext(ctx, "change events removed by retention",
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

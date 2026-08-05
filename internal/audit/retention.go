package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// APPLYING THE SCHEDULE A TENANT DECLARED.
//
// The record has been immutable since it existed and the retention period has been a column a
// tenant sets and nothing read. The surface said so out loud — it reported the schedule as
// declared and not applied — because stating a retention period the product does not enforce is
// worse than stating none: an auditor told "ninety days" reasonably concludes that day ninety-one
// is gone, and a regulator asking to see the deletion would find every row still there.
//
// The mechanism this uses is the one migration 0011 left for it. The database refuses an UPDATE, a
// DELETE and a TRUNCATE on the record outright, EXCEPT in a transaction that declares itself the
// pruner. So there is exactly one path through which a row may leave, it is named, and every other
// path is refused by the database rather than by anyone remembering.

// Bounds on one sweep.
//
// A year of a busy tenant's record is not a DELETE anybody should hold a lock for, and the writes
// that would queue behind it are audit events. So one statement removes a bounded number of rows,
// a tenant's backlog is worked through over several sweeps rather than one, and the loop stays
// interruptible throughout.
const (
	pruneBatch         = 1000
	maxBatchesPerSweep = 50
)

// Retention is one tenant's declared schedule.
type Retention struct {
	Organization tenancy.Organization
	// Days is how long the tenant says it keeps the record. It is always positive here: zero
	// declares no schedule, which means the product's default of keeping everything, and a tenant
	// that declared nothing is not reported.
	Days int
}

// Horizon is the instant before which this tenant's events have aged out.
func (r Retention) Horizon(now time.Time) time.Time {
	return now.AddDate(0, 0, -r.Days).UTC()
}

// Retentions is what the pruner needs from durable state.
//
// It is declared here rather than in the persistence package because the capability owns its
// vocabulary and persistence depends on it (ADR-017). The direction is what makes a second
// implementation — a test's, or a placement layout this build does not have — possible without
// this package learning that a database exists.
type Retentions interface {
	// DeclaredRetentions reports every tenant that has declared a schedule, across every
	// placement this deployment serves.
	//
	// It is one of the few reads that names no organization, for the same reason the
	// investigator's work scan is: its whole job is to discover which tenants there are, so
	// there is no tenant in the question to resolve a placement from. It reads no tenant data —
	// only which tenants declared a number — and every delete it leads to is tenant-scoped.
	DeclaredRetentions(ctx context.Context) ([]Retention, error)
	// PruneEventsBefore removes at most limit events older than the horizon, reporting how many
	// went. It declares itself the pruner inside its own transaction, which is the only way the
	// database permits a row to leave.
	PruneEventsBefore(ctx context.Context, organization tenancy.Organization,
		before time.Time, limit int) (int64, error)
}

// Pruner applies each tenant's declared schedule on an interval.
//
// It is the control plane acting on its own behalf, so it names no principal and writes NO audit
// event of its own. An append-only table that gained a row every time it was pruned would grow
// under exactly the mechanism meant to bound it, and the row would say nothing an operator could
// act on. What it produces instead is a log line per tenant it removed anything for.
type Pruner struct {
	Retentions Retentions
	Logger     *slog.Logger
	// Interval is how often the schedule is applied. Retention is measured in days and applying
	// it hourly is close enough to the horizon to be honest while being far enough from it that
	// nothing is spent looking.
	Interval time.Duration
	// Now is the clock. Injected so a test can put the horizon behind rows it just wrote rather
	// than waiting a day out.
	Now func() time.Time
}

// Run applies the schedule until the context is cancelled.
//
// The first sweep is on the first TICK rather than at startup. A control plane that is crash
// looping would otherwise run a scan per restart, and the schedule is measured in days — an hour's
// delay after a restart is inside the honesty this promises, and a restart storm is not.
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

// Sweep applies every declared schedule once.
//
// A failure for one tenant does not stop the others. Retention is per tenant and one placement
// being unreachable is not a reason another tenant's stated schedule goes unapplied — which is the
// difference between a degraded sweep and a silently suspended one, and the log says which.
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

// prune works one tenant's backlog down, in bounded batches.
//
// It stops early when a batch comes back short, because that is the whole backlog gone. It also
// stops at a fixed number of batches: a tenant that declares a schedule for the first time against
// years of history should have it applied over several sweeps rather than in one statement that
// holds locks while the writes that matter queue behind it.
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

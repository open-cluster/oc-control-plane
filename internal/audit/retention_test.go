package audit_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// What the pruner does with what a tenant declared.
//
// The database is not the seam here — internal/storage asserts that a row leaves only through the
// declaring transaction. What this asserts is the policy above it: which horizon each tenant's
// days resolve to, that a backlog is worked through in bounded batches rather than one, and that
// one tenant's failure does not silently suspend everybody else's schedule.

// pruned is a fake record: it remembers what it was asked to remove and answers from a backlog.
type pruned struct {
	declared []audit.Retention
	// backlog is how many aged-out events each tenant still holds.
	backlog map[string]int64
	// calls records every prune, in order, so the batching is assertable rather than inferred
	// from a total that several shapes could produce.
	calls []call
	// failFor is a tenant whose prune always fails.
	failFor string
	// listErr fails the scan itself.
	listErr error
}

type call struct {
	organization string
	before       time.Time
	limit        int
}

func (p *pruned) DeclaredRetentions(context.Context) ([]audit.Retention, error) {
	return p.declared, p.listErr
}

func (p *pruned) PruneEventsBefore(
	_ context.Context, organization tenancy.Organization, before time.Time, limit int,
) (int64, error) {
	p.calls = append(p.calls, call{organization.String(), before, limit})
	if p.failFor == organization.String() {
		return 0, errors.New("the database is unreachable")
	}
	held := p.backlog[organization.String()]
	removed := min(held, int64(limit))
	p.backlog[organization.String()] = held - removed
	return removed, nil
}

func named(t *testing.T, id string) tenancy.Organization {
	t.Helper()
	organization, err := tenancy.NewOrganization(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	return organization
}

func quietPruner(record audit.Retentions, now time.Time) audit.Pruner {
	return audit.Pruner{
		Retentions: record,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval:   time.Hour,
		Now:        func() time.Time { return now },
	}
}

func TestASweep_RemovesEventsOlderThanEachTenantsOwnHorizon(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	record := &pruned{
		declared: []audit.Retention{
			{Organization: named(t, "org-a"), Days: 30},
			{Organization: named(t, "org-b"), Days: 7},
		},
		backlog: map[string]int64{"org-a": 4, "org-b": 9},
	}

	quietPruner(record, now).Sweep(t.Context())

	horizons := make(map[string]time.Time, len(record.calls))
	for _, made := range record.calls {
		horizons[made.organization] = made.before
	}
	if want := now.AddDate(0, 0, -30); !horizons["org-a"].Equal(want) {
		t.Errorf("org-a was pruned before %s, want its own 30-day horizon %s",
			horizons["org-a"], want)
	}
	if want := now.AddDate(0, 0, -7); !horizons["org-b"].Equal(want) {
		t.Errorf("org-b was pruned before %s, want its own 7-day horizon %s",
			horizons["org-b"], want)
	}
	for tenant, left := range record.backlog {
		if left != 0 {
			t.Errorf("%s still holds %d aged-out events after a sweep", tenant, left)
		}
	}
}

// A backlog larger than one batch is worked down within the sweep, and the sweep stops as soon as
// a batch comes back short — which is how it knows there is nothing left rather than asking again.
func TestASweep_WorksABacklogDownInBoundedBatchesAndStopsWhenItIsGone(t *testing.T) {
	t.Parallel()

	record := &pruned{
		declared: []audit.Retention{{Organization: named(t, "org-a"), Days: 30}},
		backlog:  map[string]int64{"org-a": 2500},
	}

	quietPruner(record, time.Now()).Sweep(t.Context())

	if len(record.calls) != 3 {
		t.Fatalf("a backlog of 2500 took %d batches, want 3: two full and one short that ends it",
			len(record.calls))
	}
	for _, made := range record.calls {
		if made.limit <= 0 {
			t.Errorf("a batch was unbounded (limit %d); one statement must not take a lock "+
				"proportional to a backlog", made.limit)
		}
	}
	if left := record.backlog["org-a"]; left != 0 {
		t.Errorf("%d events remain after the sweep", left)
	}
}

// A first schedule declared against years of history is applied over several sweeps rather than
// in one pass, so the writes that matter are not queued behind it. What is asserted is that the
// sweep ENDS: an unbounded loop here would hold the worker forever.
func TestASweep_LeavesAnEnormousBacklogForTheNextSweepRatherThanHoldingTheWorker(t *testing.T) {
	t.Parallel()

	record := &pruned{
		declared: []audit.Retention{{Organization: named(t, "org-a"), Days: 30}},
		backlog:  map[string]int64{"org-a": 10_000_000},
	}

	quietPruner(record, time.Now()).Sweep(t.Context())

	if len(record.calls) == 0 {
		t.Fatal("the sweep removed nothing")
	}
	if left := record.backlog["org-a"]; left == 0 {
		t.Fatal("a ten-million-event backlog was removed in one sweep; the point of the bound " +
			"is that it is not")
	}
	// And the next sweep continues from where this one stopped, so the backlog does drain.
	before := record.backlog["org-a"]
	quietPruner(record, time.Now()).Sweep(t.Context())
	if record.backlog["org-a"] >= before {
		t.Error("a second sweep made no progress; a backlog that never drains is a schedule " +
			"that is never kept")
	}
}

// Retention is per tenant. One database being unreachable is not a reason another tenant's
// declared schedule goes unapplied, and the difference between a degraded sweep and a silently
// suspended one is exactly this.
func TestASweep_KeepsGoingWhenOneTenantsRecordCannotBeReached(t *testing.T) {
	t.Parallel()

	record := &pruned{
		declared: []audit.Retention{
			{Organization: named(t, "org-broken"), Days: 30},
			{Organization: named(t, "org-a"), Days: 30},
		},
		backlog: map[string]int64{"org-broken": 5, "org-a": 5},
		failFor: "org-broken",
	}

	quietPruner(record, time.Now()).Sweep(t.Context())

	if left := record.backlog["org-a"]; left != 0 {
		t.Errorf("a healthy tenant kept %d aged-out events because another tenant failed", left)
	}
}

// A tenant that declared nothing is never reported by the record, so the pruner never computes a
// horizon of "now" and never deletes a whole trail because a policy was left unset. Asserted here
// as well as in storage, because the two halves could drift apart and the consequence is total.
func TestASweep_PrunesNothingWhenNobodyHasDeclaredASchedule(t *testing.T) {
	t.Parallel()

	record := &pruned{backlog: map[string]int64{}}
	quietPruner(record, time.Now()).Sweep(t.Context())

	if len(record.calls) != 0 {
		t.Errorf("%d prunes ran with no schedule declared", len(record.calls))
	}
}

// A record that cannot be read leaves everything alone. Guessing at a horizon because the scan
// failed would delete on the strength of a query that did not answer.
func TestASweep_RemovesNothingWhenTheSchedulesCannotBeRead(t *testing.T) {
	t.Parallel()

	record := &pruned{
		backlog: map[string]int64{"org-a": 5},
		listErr: errors.New("every database is unreachable"),
	}
	quietPruner(record, time.Now()).Sweep(t.Context())

	if len(record.calls) != 0 {
		t.Errorf("%d prunes ran after the schedules could not be read", len(record.calls))
	}
}

// Run stops with its context and does not sweep before its first tick. A control plane that was
// crash looping would otherwise scan every database on every restart.
func TestThePruner_DoesNotSweepBeforeItsFirstTickAndStopsWithItsContext(t *testing.T) {
	t.Parallel()

	record := &pruned{
		declared: []audit.Retention{{Organization: named(t, "org-a"), Days: 30}},
		backlog:  map[string]int64{"org-a": 5},
	}
	pruner := quietPruner(record, time.Now())

	ctx, stop := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		pruner.Run(ctx)
	}()
	stop()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the pruner did not stop with its context")
	}
	if len(record.calls) != 0 {
		t.Errorf("%d prunes ran before the first tick", len(record.calls))
	}
}

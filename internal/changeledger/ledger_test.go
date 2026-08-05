package changeledger

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestSummary_ReadsAsWhatMovedWithBothValues(t *testing.T) {
	entry := Entry{
		Kind: KindDeployment, Name: "api", Change: ChangeModified,
		Fields: []FieldChange{{
			Field:  "spec.template.spec.containers[app].resources.limits.memory",
			Before: "256Mi", After: "128Mi",
		}},
	}
	summary := entry.Summary()
	want := "deployment api: spec.template.spec.containers[app].resources.limits.memory 256Mi -> 128Mi"
	if summary != want {
		t.Fatalf("a deploy must read as what moved, got %q", summary)
	}
}

func TestSummary_IsBoundedHoweverManyFieldsMoved(t *testing.T) {
	entry := Entry{Kind: KindDeployment, Name: "api", Change: ChangeModified}
	for range 10 {
		entry.Fields = append(entry.Fields, FieldChange{Field: "spec.replicas", Before: "1", After: "2"})
	}
	summary := entry.Summary()
	if len(summary) > 400 || !containsSuffix(summary, ", and more") {
		t.Fatalf("a mass change must summarize, not enumerate, got %q", summary)
	}
}

func TestSummary_ADeletionAndACreationSayWhatHappened(t *testing.T) {
	deleted := Entry{Kind: KindSecret, Name: "db-credentials", Change: ChangeDeleted}
	if deleted.Summary() != "secret db-credentials was deleted" {
		t.Fatalf("got %q", deleted.Summary())
	}
	created := Entry{Kind: KindConfigMap, Name: "settings", Change: ChangeCreated}
	if created.Summary() != "configmap settings was created" {
		t.Fatalf("got %q", created.Summary())
	}
}

// fakeRetention scripts what each prune call removes.
type fakeRetention struct {
	batches []int64
	calls   int
	horizon time.Time
	err     error
}

func (f *fakeRetention) PruneChangeLedgerBefore(
	_ context.Context, before time.Time, _ int,
) (int64, error) {
	f.calls++
	f.horizon = before
	if f.err != nil {
		return 0, f.err
	}
	if f.calls > len(f.batches) {
		return 0, nil
	}
	return f.batches[f.calls-1], nil
}

func TestPruner_StopsWhenABatchComesBackShort(t *testing.T) {
	retention := &fakeRetention{batches: []int64{pruneBatch, 12}}
	pruner := Pruner{
		Retention: retention,
		Logger:    slog.Default(),
		Days:      90,
		Now:       func() time.Time { return time.Date(2026, 8, 5, 3, 40, 0, 0, time.UTC) },
	}
	pruner.Sweep(context.Background())

	if retention.calls != 2 {
		t.Fatalf("a short batch is the backlog gone; got %d calls", retention.calls)
	}
	want := time.Date(2026, 5, 7, 3, 40, 0, 0, time.UTC)
	if !retention.horizon.Equal(want) {
		t.Fatalf("the horizon must be the retention's own age, got %v want %v",
			retention.horizon, want)
	}
}

func TestPruner_AFailedSweepStopsRatherThanRetryingForever(t *testing.T) {
	retention := &fakeRetention{err: errors.New("placement unreachable")}
	pruner := Pruner{Retention: retention, Logger: slog.Default(), Days: 90}
	pruner.Sweep(context.Background())
	if retention.calls != 1 {
		t.Fatalf("a failed batch must end the sweep, got %d calls", retention.calls)
	}
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

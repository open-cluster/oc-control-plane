package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The change ledger's storage contract: at-least-once deltas collapse instead of
// duplicating history, a Connection another relay serves is refused, baselines never
// read as changes, and the coverage boundary moves exactly when a gap in watching held
// a change.

func ledgerScope(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
) (registration, integration uuid.UUID) {
	t.Helper()
	registration = enrolledRelay(t, placements, organization)
	integration = kubernetesIntegration(t, placements, organization, registration)
	scopes, err := placements.OpenInventoryScopes(
		context.Background(), organization, registration, 5*time.Minute)
	if err != nil {
		t.Fatalf("opening inventory scopes: %v", err)
	}
	found := false
	for _, scope := range scopes {
		if scope.IntegrationID == integration {
			found = true
		}
	}
	if !found {
		t.Fatalf("the kubernetes integration %s must gain a scope", integration)
	}
	return registration, integration
}

func imageChange(uid, revision, before, after string) changeledger.Change {
	return changeledger.Change{
		Namespace: "shop", Kind: changeledger.KindDeployment, Name: "api", UID: uid,
		ObservedRevision: revision, Change: changeledger.ChangeModified,
		Fields: []changeledger.FieldChange{{
			Field: "spec.template.spec.containers[app].image", Before: before, After: after,
		}},
	}
}

func TestChangeLedger_ARedeliveredDeltaRecordsNothingTwice(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)

	delta := changeledger.Delta{
		IntegrationID: integration,
		ObservedAt:    time.Now().UTC(),
		Changes:       []changeledger.Change{imageChange("uid-1", "g4.aaa", "app:v1", "app:v2")},
	}
	first, err := placements.RecordInventoryDelta(
		context.Background(), organization, registration, delta)
	if err != nil || first.Inserted != 1 {
		t.Fatalf("the first delivery must record one entry, got %+v, %v", first, err)
	}
	second, err := placements.RecordInventoryDelta(
		context.Background(), organization, registration, delta)
	if err != nil || second.Inserted != 0 {
		t.Fatalf("a redelivery must collapse, got %+v, %v", second, err)
	}
}

func TestChangeLedger_ADeltaNamingAConnectionAnotherRelayServesIsRefused(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	_, integration := ledgerScope(t, placements, organization)
	stranger := enrolledRelay(t, placements, organization)

	recorded, err := placements.RecordInventoryDelta(
		context.Background(), organization, stranger, changeledger.Delta{
			IntegrationID: integration,
			ObservedAt:    time.Now().UTC(),
			Changes:       []changeledger.Change{imageChange("uid-1", "g4.aaa", "a", "b")},
		})
	if err != nil {
		t.Fatalf("a refusal is an answer, not an error: %v", err)
	}
	if !recorded.Refused || recorded.Inserted != 0 {
		t.Fatalf("a relay must not write history through an integration it does not serve, got %+v",
			recorded)
	}
}

func TestChangeLedger_TheWindowAnswersChangesAndNeverBaselines(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)
	ctx := context.Background()

	// Postgres keeps microseconds; a nanosecond-precise instant would fail an equality it
	// deserves to pass.
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	baseline := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changeledger.Change{{
			Namespace: "shop", Kind: changeledger.KindDeployment, Name: "api", UID: "uid-1",
			ObservedRevision: "g3.aaa", Change: changeledger.ChangeCreated,
			Fields: []changeledger.FieldChange{{
				Field: "spec.template.spec.containers[app].image", After: "app:v1",
			}},
		}},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, baseline); err != nil {
		t.Fatalf("recording the baseline: %v", err)
	}
	change := changeledger.Delta{
		IntegrationID: integration, ObservedAt: start.Add(30 * time.Minute),
		Changes: []changeledger.Change{
			imageChange("uid-1", "g4.bbb", "app:v1", "app:v2"),
			{
				Namespace: "other", Kind: changeledger.KindDeployment, Name: "elsewhere",
				UID: "uid-9", ObservedRevision: "g2.ccc", Change: changeledger.ChangeModified,
			},
		},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, change); err != nil {
		t.Fatalf("recording the change: %v", err)
	}

	answer, err := placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		start.Add(-time.Minute), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if !answer.Covered {
		t.Fatal("a scope with a baseline is covered")
	}
	if len(answer.Entries) != 1 {
		t.Fatalf("the window holds one change in this namespace — the baseline is where "+
			"watching began, not something changing, and 'other' is another namespace — got %d",
			len(answer.Entries))
	}
	entry := answer.Entries[0]
	if entry.Change != changeledger.ChangeModified || entry.UID != "uid-1" {
		t.Fatalf("the change must survive intact, got %+v", entry)
	}
	if len(entry.Fields) != 1 || entry.Fields[0].Before != "app:v1" || entry.Fields[0].After != "app:v2" {
		t.Fatalf("both values must survive, got %+v", entry.Fields)
	}
	if entry.RecordedAt.IsZero() || !entry.ObservedAt.Equal(change.ObservedAt) {
		t.Fatalf("both clocks must survive: a delayed delivery must stay distinguishable "+
			"from a delayed change, got observed %v recorded %v", entry.ObservedAt, entry.RecordedAt)
	}
}

func TestChangeLedger_ACollapsedRebaselinePreservesTheCoverageBoundary(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)
	ctx := context.Background()

	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	object := changeledger.Change{
		Namespace: "shop", Kind: changeledger.KindDeployment, Name: "api", UID: "uid-1",
		ObservedRevision: "g3.aaa", Change: changeledger.ChangeCreated,
	}
	first := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changeledger.Change{object},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, first); err != nil {
		t.Fatalf("recording the first baseline: %v", err)
	}

	// A quick restart re-baselines within the scope's own cadence (two requested intervals
	// past the last confirmation; the scope asks for five minutes). Every row collapses, so
	// no watched field moved, and the short gap bounds what a collapse cannot prove — the
	// boundary survives.
	quiet := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(8 * time.Minute),
		Changes: []changeledger.Change{object},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, quiet); err != nil {
		t.Fatalf("recording the quiet re-baseline: %v", err)
	}
	answer, err := placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		start, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reading the scope: %v", err)
	}
	if answer.Scope.CoveredSince == nil || !answer.Scope.CoveredSince.Equal(start) {
		t.Fatalf("a collapsed re-baseline within the cadence must preserve covered-since, got %v",
			answer.Scope.CoveredSince)
	}

	// A LONG silence moves the boundary even when everything collapsed: a collapse proves
	// no watched field moved, and cannot prove an object was not deleted and mourned by
	// nobody while the Relay was away.
	longGap := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(time.Hour),
		Changes: []changeledger.Change{object},
	}
	if _, err = placements.RecordInventoryDelta(ctx, organization, registration, longGap); err != nil {
		t.Fatalf("recording the long-gap re-baseline: %v", err)
	}
	answer, err = placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		start, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("re-reading the scope: %v", err)
	}
	if answer.Scope.CoveredSince == nil || !answer.Scope.CoveredSince.Equal(longGap.ObservedAt) {
		t.Fatalf("an hour of silence cannot be vouched for by a collapse; covered-since "+
			"must move to where watching resumed, got %v", answer.Scope.CoveredSince)
	}

	// A re-baseline that finds anything new proves the gap held a change, and the boundary
	// moves regardless of how short the gap was.
	moved := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(65 * time.Minute),
		Changes: []changeledger.Change{{
			Namespace: "shop", Kind: changeledger.KindDeployment, Name: "api", UID: "uid-1",
			ObservedRevision: "g4.bbb", Change: changeledger.ChangeCreated,
		}},
	}
	if _, err = placements.RecordInventoryDelta(ctx, organization, registration, moved); err != nil {
		t.Fatalf("recording the discontinuous re-baseline: %v", err)
	}
	answer, err = placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		start, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("re-reading the scope: %v", err)
	}
	if answer.Scope.CoveredSince == nil || !answer.Scope.CoveredSince.Equal(moved.ObservedAt) {
		t.Fatalf("a discontinuous re-baseline must move covered-since to where watching "+
			"resumed, got %v", answer.Scope.CoveredSince)
	}
}

func TestChangeLedger_FreshnessStampsAdvanceTheScope(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)
	ctx := context.Background()

	confirmed := time.Now().UTC().Truncate(time.Second)
	err := placements.RecordInventoryFreshness(ctx, organization, registration,
		[]changeledger.Freshness{{
			IntegrationID: integration, CompletedAt: &confirmed, Faulted: false, Truncated: true,
		}})
	if err != nil {
		t.Fatalf("recording freshness: %v", err)
	}
	answer, err := placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		confirmed.Add(-time.Hour), confirmed, 10)
	if err != nil {
		t.Fatalf("reading the scope: %v", err)
	}
	if answer.Scope.LastConfirmedAt == nil || !answer.Scope.LastConfirmedAt.Equal(confirmed) {
		t.Fatalf("the stamp must land, got %v", answer.Scope.LastConfirmedAt)
	}
	if !answer.Scope.Truncated {
		t.Fatal("a truncated tick must be visible on the scope")
	}

	// A stranger's stamp for this integration changes nothing: the guard is the same one
	// deltas pass through.
	stranger := enrolledRelay(t, placements, organization)
	later := confirmed.Add(time.Hour)
	err = placements.RecordInventoryFreshness(ctx, organization, stranger,
		[]changeledger.Freshness{{IntegrationID: integration, CompletedAt: &later}})
	if err != nil {
		t.Fatalf("recording a stranger's freshness: %v", err)
	}
	answer, err = placements.RecentLedgerChanges(ctx, organization, integration, "shop",
		confirmed.Add(-time.Hour), confirmed, 10)
	if err != nil {
		t.Fatalf("re-reading the scope: %v", err)
	}
	if !answer.Scope.LastConfirmedAt.Equal(confirmed) {
		t.Fatalf("a relay that does not serve the integration must not confirm it, got %v",
			answer.Scope.LastConfirmedAt)
	}
}

func TestChangeLedger_RetentionPrunesByAgeAndOnlyByAge(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)
	ctx := context.Background()

	old := changeledger.Delta{
		IntegrationID: integration, ObservedAt: time.Now().UTC().Add(-time.Hour),
		Changes: []changeledger.Change{imageChange("uid-1", "g4.aaa", "a", "b")},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, old); err != nil {
		t.Fatalf("recording: %v", err)
	}

	// received_at is the transaction clock, so "older than now" ages the row out and
	// "older than an hour ago" does not.
	removed, err := placements.PruneChangeLedgerBefore(ctx, time.Now().UTC().Add(-time.Minute), 100)
	if err != nil || removed != 0 {
		t.Fatalf("nothing has aged out yet, got %d, %v", removed, err)
	}
	removed, err = placements.PruneChangeLedgerBefore(ctx, time.Now().UTC().Add(time.Minute), 100)
	if err != nil || removed != 1 {
		t.Fatalf("the aged entry must go, got %d, %v", removed, err)
	}
}

func TestChangeLedger_WorkloadInventoryIsACurrentBoundedDigest(t *testing.T) {
	placements, organization := migratedPlacement(t)
	defer placements.Close()
	registration, integration := ledgerScope(t, placements, organization)
	ctx := context.Background()

	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	delta := changeledger.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changeledger.Change{
			{Namespace: "shop", Kind: changeledger.KindDeployment, Name: "api",
				UID: "uid-1", ObservedRevision: "g1.a", Change: changeledger.ChangeCreated},
			{Namespace: "shop", Kind: changeledger.KindStatefulSet, Name: "queue",
				UID: "uid-2", ObservedRevision: "g1.b", Change: changeledger.ChangeCreated},
			{Namespace: "shop", Kind: changeledger.KindConfigMap, Name: "settings",
				UID: "uid-3", ObservedRevision: "g1.c", Change: changeledger.ChangeCreated},
		},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, delta); err != nil {
		t.Fatalf("recording the baseline: %v", err)
	}
	gone := changeledger.Delta{
		IntegrationID: integration, ObservedAt: start.Add(10 * time.Minute),
		Changes: []changeledger.Change{{
			Namespace: "shop", Kind: changeledger.KindStatefulSet, Name: "queue",
			UID: "uid-2", ObservedRevision: "", Change: changeledger.ChangeDeleted,
		}},
	}
	if _, err := placements.RecordInventoryDelta(ctx, organization, registration, gone); err != nil {
		t.Fatalf("recording the deletion: %v", err)
	}

	digest, err := placements.WorkloadInventory(ctx, organization, 10)
	if err != nil {
		t.Fatalf("reading the inventory: %v", err)
	}
	if len(digest) != 1 || digest[0] != "shop/deployment api" {
		t.Fatalf("digest = %v; deletions drop out and only workload kinds appear", digest)
	}

	if bounded, err := placements.WorkloadInventory(ctx, organization, 0); err != nil ||
		len(bounded) != 0 {
		t.Fatalf("a zero bound reads nothing: %v %v", bounded, err)
	}
}

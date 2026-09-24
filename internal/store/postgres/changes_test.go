package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/changes"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// The Changes capability's storage contract: at-least-once deltas collapse instead of
// duplicating history, an Integration another Relay serves is refused, baselines never
// read as changes, and the coverage boundary moves exactly when a gap in watching held
// a change.

func changeScope(
	t *testing.T, database *storage.Database, organization uuid.UUID,
) (registration, integration uuid.UUID) {
	t.Helper()
	registration = enrolledRelay(t, database, organization)
	integration = kubernetesIntegration(t, database, organization, registration)
	scopes, err := database.OpenInventoryScopes(
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

func imageChange(uid, revision, before, after string) changes.Change {
	return changes.Change{
		Namespace: "shop", Kind: changes.KindDeployment, Name: "api", UID: uid,
		ObservedRevision: revision, Change: changes.ChangeModified,
		Fields: []changes.FieldChange{{
			Field: "spec.template.spec.containers[app].image", Before: before, After: after,
		}},
	}
}

func TestChanges_ARedeliveredDeltaRecordsNothingTwice(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)

	delta := changes.Delta{
		IntegrationID: integration,
		ObservedAt:    time.Now().UTC(),
		Changes:       []changes.Change{imageChange("uid-1", "g4.aaa", "app:v1", "app:v2")},
	}
	first, err := database.RecordInventoryDelta(
		context.Background(), organization, registration, delta)
	if err != nil || first.Inserted != 1 {
		t.Fatalf("the first delivery must record one event, got %+v, %v", first, err)
	}
	second, err := database.RecordInventoryDelta(
		context.Background(), organization, registration, delta)
	if err != nil || second.Inserted != 0 {
		t.Fatalf("a redelivery must collapse, got %+v, %v", second, err)
	}
}

func TestChanges_ADeltaNamingAnIntegrationAnotherRelayServesIsRefused(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	_, integration := changeScope(t, database, organization)
	stranger := enrolledRelay(t, database, organization)

	recorded, err := database.RecordInventoryDelta(
		context.Background(), organization, stranger, changes.Delta{
			IntegrationID: integration,
			ObservedAt:    time.Now().UTC(),
			Changes:       []changes.Change{imageChange("uid-1", "g4.aaa", "a", "b")},
		})
	if err != nil {
		t.Fatalf("a refusal is an answer, not an error: %v", err)
	}
	if !recorded.Refused || recorded.Inserted != 0 {
		t.Fatalf("a relay must not write history through an integration it does not serve, got %+v",
			recorded)
	}
}

func TestChanges_TheWindowAnswersChangesAndNeverBaselines(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)
	ctx := context.Background()

	// Postgres keeps microseconds; a nanosecond-precise instant would fail an equality it
	// deserves to pass.
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	baseline := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changes.Change{{
			Namespace: "shop", Kind: changes.KindDeployment, Name: "api", UID: "uid-1",
			ObservedRevision: "g3.aaa", Change: changes.ChangeCreated,
			Fields: []changes.FieldChange{{
				Field: "spec.template.spec.containers[app].image", After: "app:v1",
			}},
		}},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, baseline); err != nil {
		t.Fatalf("recording the baseline: %v", err)
	}
	change := changes.Delta{
		IntegrationID: integration, ObservedAt: start.Add(30 * time.Minute),
		Changes: []changes.Change{
			imageChange("uid-1", "g4.bbb", "app:v1", "app:v2"),
			{
				Namespace: "other", Kind: changes.KindDeployment, Name: "elsewhere",
				UID: "uid-9", ObservedRevision: "g2.ccc", Change: changes.ChangeModified,
			},
		},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, change); err != nil {
		t.Fatalf("recording the change: %v", err)
	}

	answer, err := database.RecentChanges(ctx, organization, integration, "shop",
		start.Add(-time.Minute), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if !answer.Covered {
		t.Fatal("a scope with a baseline is covered")
	}
	if len(answer.Events) != 1 {
		t.Fatalf("the window holds one change in this namespace — the baseline is where "+
			"watching began, not something changing, and 'other' is another namespace — got %d",
			len(answer.Events))
	}
	event := answer.Events[0]
	if event.Change != changes.ChangeModified || event.UID != "uid-1" {
		t.Fatalf("the change must survive intact, got %+v", event)
	}
	if len(event.Fields) != 1 || event.Fields[0].Before != "app:v1" || event.Fields[0].After != "app:v2" {
		t.Fatalf("both values must survive, got %+v", event.Fields)
	}
	if event.RecordedAt.IsZero() || !event.ObservedAt.Equal(change.ObservedAt) {
		t.Fatalf("both clocks must survive: a delayed delivery must stay distinguishable "+
			"from a delayed change, got observed %v recorded %v", event.ObservedAt, event.RecordedAt)
	}
}

func TestChanges_ACollapsedRebaselinePreservesTheCoverageBoundary(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)
	ctx := context.Background()

	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	object := changes.Change{
		Namespace: "shop", Kind: changes.KindDeployment, Name: "api", UID: "uid-1",
		ObservedRevision: "g3.aaa", Change: changes.ChangeCreated,
	}
	first := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changes.Change{object},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, first); err != nil {
		t.Fatalf("recording the first baseline: %v", err)
	}

	// A quick restart re-baselines within the scope's own cadence (two requested intervals
	// past the last confirmation; the scope asks for five minutes). Every row collapses, so
	// no watched field moved, and the short gap bounds what a collapse cannot prove — the
	// boundary survives.
	quiet := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(8 * time.Minute),
		Changes: []changes.Change{object},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, quiet); err != nil {
		t.Fatalf("recording the quiet re-baseline: %v", err)
	}
	answer, err := database.RecentChanges(ctx, organization, integration, "shop",
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
	longGap := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(time.Hour),
		Changes: []changes.Change{object},
	}
	if _, err = database.RecordInventoryDelta(ctx, organization, registration, longGap); err != nil {
		t.Fatalf("recording the long-gap re-baseline: %v", err)
	}
	answer, err = database.RecentChanges(ctx, organization, integration, "shop",
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
	moved := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start.Add(65 * time.Minute),
		Changes: []changes.Change{{
			Namespace: "shop", Kind: changes.KindDeployment, Name: "api", UID: "uid-1",
			ObservedRevision: "g4.bbb", Change: changes.ChangeCreated,
		}},
	}
	if _, err = database.RecordInventoryDelta(ctx, organization, registration, moved); err != nil {
		t.Fatalf("recording the discontinuous re-baseline: %v", err)
	}
	answer, err = database.RecentChanges(ctx, organization, integration, "shop",
		start, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("re-reading the scope: %v", err)
	}
	if answer.Scope.CoveredSince == nil || !answer.Scope.CoveredSince.Equal(moved.ObservedAt) {
		t.Fatalf("a discontinuous re-baseline must move covered-since to where watching "+
			"resumed, got %v", answer.Scope.CoveredSince)
	}
}

func TestChanges_FreshnessStampsAdvanceTheScope(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)
	ctx := context.Background()

	confirmed := time.Now().UTC().Truncate(time.Second)
	err := database.RecordInventoryFreshness(ctx, organization, registration,
		[]changes.Freshness{{
			IntegrationID: integration, CompletedAt: &confirmed, Faulted: false, Truncated: true,
		}})
	if err != nil {
		t.Fatalf("recording freshness: %v", err)
	}
	answer, err := database.RecentChanges(ctx, organization, integration, "shop",
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
	stranger := enrolledRelay(t, database, organization)
	later := confirmed.Add(time.Hour)
	err = database.RecordInventoryFreshness(ctx, organization, stranger,
		[]changes.Freshness{{IntegrationID: integration, CompletedAt: &later}})
	if err != nil {
		t.Fatalf("recording a stranger's freshness: %v", err)
	}
	answer, err = database.RecentChanges(ctx, organization, integration, "shop",
		confirmed.Add(-time.Hour), confirmed, 10)
	if err != nil {
		t.Fatalf("re-reading the scope: %v", err)
	}
	if !answer.Scope.LastConfirmedAt.Equal(confirmed) {
		t.Fatalf("a relay that does not serve the integration must not confirm it, got %v",
			answer.Scope.LastConfirmedAt)
	}
}

func TestChanges_RetentionPrunesByAgeAndOnlyByAge(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)
	ctx := context.Background()

	old := changes.Delta{
		IntegrationID: integration, ObservedAt: time.Now().UTC().Add(-time.Hour),
		Changes: []changes.Change{imageChange("uid-1", "g4.aaa", "a", "b")},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, old); err != nil {
		t.Fatalf("recording: %v", err)
	}

	// received_at is the transaction clock, so "older than now" ages the row out and
	// "older than an hour ago" does not.
	removed, err := database.PruneChangesBefore(ctx, time.Now().UTC().Add(-time.Minute), 100)
	if err != nil || removed != 0 {
		t.Fatalf("nothing has aged out yet, got %d, %v", removed, err)
	}
	removed, err = database.PruneChangesBefore(ctx, time.Now().UTC().Add(time.Minute), 100)
	if err != nil || removed != 1 {
		t.Fatalf("the aged event must go, got %d, %v", removed, err)
	}
}

func TestChanges_WorkloadInventoryIsACurrentBoundedDigest(t *testing.T) {
	database, organization := migratedDatabase(t)
	defer database.Close()
	registration, integration := changeScope(t, database, organization)
	ctx := context.Background()
	otherIntegration := kubernetesIntegration(t, database, organization, registration)
	if _, err := database.OpenInventoryScopes(ctx, organization, registration, 5*time.Minute); err != nil {
		t.Fatalf("opening the second inventory scope: %v", err)
	}

	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	delta := changes.Delta{
		IntegrationID: integration, Baseline: true, ObservedAt: start,
		Changes: []changes.Change{
			{Namespace: "shop", Kind: changes.KindDeployment, Name: "api",
				UID: "uid-1", ObservedRevision: "g1.a", Change: changes.ChangeCreated},
			{Namespace: "shop", Kind: changes.KindStatefulSet, Name: "queue",
				UID: "uid-2", ObservedRevision: "g1.b", Change: changes.ChangeCreated},
			{Namespace: "shop", Kind: changes.KindConfigMap, Name: "settings",
				UID: "uid-3", ObservedRevision: "g1.c", Change: changes.ChangeCreated},
		},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, delta); err != nil {
		t.Fatalf("recording the baseline: %v", err)
	}
	other := changes.Delta{
		IntegrationID: otherIntegration, Baseline: true, ObservedAt: start,
		Changes: []changes.Change{{
			Namespace: "shop", Kind: changes.KindDeployment, Name: "api",
			UID: "other-uid", ObservedRevision: "g1.other", Change: changes.ChangeCreated,
		}},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, other); err != nil {
		t.Fatalf("recording the second baseline: %v", err)
	}
	duplicates, err := database.WorkloadInventory(ctx, organization, 10)
	if err != nil {
		t.Fatalf("reading duplicate inventory: %v", err)
	}
	if len(duplicates) != 3 ||
		!inventoryLine(duplicates, integration, "shop/deployment api") ||
		!inventoryLine(duplicates, otherIntegration, "shop/deployment api") ||
		!inventoryLine(duplicates, integration, "shop/statefulset queue") {
		t.Fatalf("duplicate inventory = %v", duplicates)
	}
	deletedDuplicate := changes.Delta{
		IntegrationID: otherIntegration, ObservedAt: start.Add(5 * time.Minute),
		Changes: []changes.Change{{
			Namespace: "shop", Kind: changes.KindDeployment, Name: "api",
			UID: "other-uid", ObservedRevision: "", Change: changes.ChangeDeleted,
		}},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, deletedDuplicate); err != nil {
		t.Fatalf("recording the duplicate deletion: %v", err)
	}
	gone := changes.Delta{
		IntegrationID: integration, ObservedAt: start.Add(10 * time.Minute),
		Changes: []changes.Change{{
			Namespace: "shop", Kind: changes.KindStatefulSet, Name: "queue",
			UID: "uid-2", ObservedRevision: "", Change: changes.ChangeDeleted,
		}},
	}
	if _, err := database.RecordInventoryDelta(ctx, organization, registration, gone); err != nil {
		t.Fatalf("recording the deletion: %v", err)
	}

	digest, err := database.WorkloadInventory(ctx, organization, 10)
	if err != nil {
		t.Fatalf("reading the inventory: %v", err)
	}
	if len(digest) != 1 || !inventoryLine(digest, integration, "shop/deployment api") ||
		inventoryLine(digest, otherIntegration, "shop/deployment api") {
		t.Fatalf("digest = %v; same-name deletion must affect only its source Integration", digest)
	}

	if bounded, err := database.WorkloadInventory(ctx, organization, 0); err != nil ||
		len(bounded) != 0 {
		t.Fatalf("a zero bound reads nothing: %v %v", bounded, err)
	}
}

func inventoryLine(lines []string, integration uuid.UUID, workload string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, "integration "+integration.String()+" covered since ") &&
			strings.Contains(line, " last confirmed ") &&
			strings.HasSuffix(line, " "+workload) {
			return true
		}
	}
	return false
}

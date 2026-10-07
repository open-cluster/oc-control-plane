package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-cluster/oc-control-plane/internal/changes"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

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

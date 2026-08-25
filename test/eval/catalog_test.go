package eval_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/test/eval"
)

func TestScenarioLoadsIndependentFilesAndRebasesOperationalTimestamps(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"case.yaml", "alert.json", "kubernetes.json", "github.json", "slack.json", "truth.yaml"} {
		if _, err := os.Stat(filepath.Join("cases", "single-root-cause", name)); err != nil {
			t.Fatalf("the single-root-cause scenario is missing its independent %s fixture: %v", name, err)
		}
	}
	now := time.Date(2030, 1, 2, 15, 4, 5, 0, time.UTC)
	scenarios, err := eval.LoadCases(now)
	if err != nil {
		t.Fatalf("loading independent scenario files: %v", err)
	}
	for _, scenario := range scenarios {
		if scenario.Name != "single-root-cause" {
			continue
		}
		if scenario.Alertname != "CheckoutLatencyHigh" || len(scenario.Truth.Causes) != 1 ||
			len(scenario.Kubernetes.Workloads) != 1 || scenario.Kubernetes.Workloads[0].Name != "payments" {
			t.Fatalf("scenario files did not supply independent alert and truth data: %+v", scenario)
		}
		for _, workspace := range scenario.Workspaces {
			for _, channel := range workspace.Channels {
				for _, message := range channel.Messages {
					if message.At.Before(now.Add(-3*time.Hour)) || message.At.After(now) {
						t.Fatalf("scenario message was not rebased into its investigation window: %s", message.At)
					}
				}
			}
		}
		return
	}
	t.Fatal("the scenario loader omitted single-root-cause")
}

func TestCatalogProvidesVersionedGroundTruth(t *testing.T) {
	fixtures, err := eval.Load()
	if err != nil {
		t.Fatal(err)
	}
	fixture, ok := fixtures.Lookup("single-root-cause")
	if !ok || fixture.Revision == "" || len(fixture.GroundTruth.Causes) != 1 {
		t.Fatalf("single-root-cause fixture has no versioned ground truth: %+v", fixture)
	}
	if worlds := eval.Cases(time.Now()); len(worlds) != len(fixtures.Fixtures) {
		t.Fatalf("resolved %d executable worlds for %d fixtures", len(worlds), len(fixtures.Fixtures))
	}
}

func TestConversationEvaluationMeasuresFindingContinuityWithoutCompaction(t *testing.T) {
	t.Parallel()

	catalog, err := eval.Load()
	if err != nil {
		t.Fatal(err)
	}
	fixture, found := catalog.Lookup("conversation-memory-across-bounded-history")
	if !found {
		t.Fatal("the evaluation catalog must include bounded-history conversation continuity")
	}
	if len(fixture.GroundTruth.Survives) == 0 {
		t.Fatal("the bounded-history fixture must identify cited findings that survive follow-up turns")
	}
}

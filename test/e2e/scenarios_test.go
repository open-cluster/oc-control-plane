package e2e

import (
	"context"
	"testing"
	"time"
)

// Every scenario actually reaches its declared broken state, against a real cluster.
//
// This is the first of the harness's own testing requirements, and it cannot be answered by a
// fake: what is in question is whether a manifest produces the failure it claims to. A scenario
// whose readiness condition never holds would discard every run of it, and the set would quietly
// shrink from ten to nine without anyone deciding that.
//
// It runs all ten against ONE cluster, which a real run does not do — a run gives each scenario a
// cluster of its own so nothing one scenario did can reach another. The namespaces are separate
// either way, and what is being checked here is the manifest and the readiness condition rather
// than the isolation.
func TestScenarios_EveryScenarioReachesItsDeclaredState(t *testing.T) {
	if testing.Short() {
		t.Skip("provisioning every scenario: requires a Docker daemon")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	kubernetes, err := startCluster(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("starting the cluster: %v", err)
	}
	t.Cleanup(kubernetes.close)

	for _, scenario := range Scenarios() {
		t.Run(scenario.ID, func(t *testing.T) {
			started := time.Now()
			if err := scenario.Prepare(ctx, kubernetes.client); err != nil {
				t.Fatalf("the cluster did not reach the declared state: %v", err)
			}
			t.Logf("reached %q in %s", scenario.Title, time.Since(started).Round(time.Second))
		})
	}
}

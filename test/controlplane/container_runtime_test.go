package controlplane

import (
	"os"
	"testing"
)

const requireContainers = "OC_REQUIRE_CONTAINERS"

func noContainerRuntime(t *testing.T, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("no container runtime is reachable and %s is set, so this suite must not "+
			"report success without having run: %v", requireContainers, err)
	}
	t.Skipf("cannot start postgres (is the Docker daemon reachable?): %v", err)
}

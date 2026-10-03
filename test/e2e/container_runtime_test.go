package e2e

import (
	"os"
	"testing"
)

const requireContainers = "OC_REQUIRE_CONTAINERS"

func noContainerRuntime(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("end-to-end proof: %s, and %s is set, so this module must not report "+
			"success without having run: %v", what, requireContainers, err)
	}
	t.Skipf("end-to-end proof: %s: %v", what, err)
}

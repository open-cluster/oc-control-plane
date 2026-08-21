package e2e

import (
	"os"
	"testing"
)

// requireContainers is the variable CI sets to say a container runtime is guaranteed here.
//
// This module exists to run two real implementations against real infrastructure. A run of
// it that proves nothing is worse than no run at all, because it reports the same green as
// one that proved everything — and this module is the one whose exit code is most likely to
// be read as coverage it does not have.
//
// The short-mode guard above stays a skip: -short is a contributor saying they do not want
// this, which is a different thing from an environment failing to provide it.
const requireContainers = "OC_REQUIRE_CONTAINERS"

// noContainerRuntime ends a proof that needs a container runtime and could not reach one:
// fatal where one was promised, skipped where none was. The condition is named because
// "not installed" and "installed but unhealthy" send whoever reads it to different places.
func noContainerRuntime(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("end-to-end proof: %s, and %s is set, so this module must not report "+
			"success without having run: %v", what, requireContainers, err)
	}
	t.Skipf("end-to-end proof: %s: %v", what, err)
}

package controlplane

import (
	"os"
	"testing"
)

// requireContainers is the variable CI sets to say a container runtime is guaranteed here.
//
// Every test in this package that reaches a real database starts a container, and every one
// of them gives up when it cannot. On a contributor's machine that is the honest answer —
// the short suite is the supported way to work without a runtime, and a skip says so. In CI
// it is the wrong answer: a runner whose runtime never came up would report success over a
// suite that never executed, and nothing in the output would say which of the two happened.
//
// An image that cannot be PULLED already fails loudly, because by then a container has
// started and a runtime demonstrably exists. A daemon that was never there did not. This
// closes that asymmetry and adds no new kind of check.
const requireContainers = "OC_REQUIRE_CONTAINERS"

// noContainerRuntime ends a test that needs a container runtime and could not reach one:
// fatal where one was promised, skipped where none was.
func noContainerRuntime(t *testing.T, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("no container runtime is reachable and %s is set, so this suite must not "+
			"report success without having run: %v", requireContainers, err)
	}
	t.Skipf("cannot start postgres (is the Docker daemon reachable?): %v", err)
}

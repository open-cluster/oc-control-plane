package storage_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireContainers is the variable CI sets to say a container runtime is guaranteed here.
// The reasoning is the same as the composition root's copy of this, and the two are
// deliberately separate: they are different packages, and one of the three places this
// pattern lives is a different module entirely, so a shared helper would have to leave
// internal/ to reach it. Eight lines repeated is the smaller cost.
const requireContainers = "OC_REQUIRE_CONTAINERS"

// noContainerRuntime ends a test that needs a container runtime and could not reach one:
// fatal where one was promised, skipped where none was. Without the fatal case, a CI run
// whose runtime never came up reports success over a suite that never executed.
func noContainerRuntime(t *testing.T, err error) {
	t.Helper()
	if os.Getenv(requireContainers) != "" {
		t.Fatalf("no container runtime is reachable and %s is set, so this suite must not "+
			"report success without having run: %v", requireContainers, err)
	}
	t.Skipf("cannot start postgres (is the Docker daemon reachable?): %v", err)
}

// The probe below is how the two outcomes are DEMONSTRATED rather than reasoned about.
//
// A daemon that is not there cannot be simulated by configuration on every platform —
// Docker Desktop resolves its own endpoint and ignores DOCKER_HOST — so the failure is
// injected directly and the two outcomes are observed from outside, in a subprocess,
// where a Fatalf is an exit code rather than something this process would die of.
const runtimeProbe = "OC_CONTAINER_RUNTIME_PROBE"

// TestNoContainerRuntimeProbe is not a test. It is the body the two cases below run.
func TestNoContainerRuntimeProbe(t *testing.T) {
	if os.Getenv(runtimeProbe) != "1" {
		t.Skip("probe: run by TestAMissingRuntime… through a subprocess")
	}
	noContainerRuntime(t, errors.New("injected: no runtime"))
}

func TestAMissingRuntimeSkipsWhenNoneWasPromised(t *testing.T) {
	t.Parallel()

	output, err := runProbe(t, "")
	if err != nil {
		t.Fatalf("the probe failed where it should have skipped: %v\n%s", err, output)
	}
	if !strings.Contains(output, "cannot start postgres") {
		t.Errorf("a skipped probe does not say why:\n%s", output)
	}
}

func TestAMissingRuntimeFailsWhenContainersWereRequired(t *testing.T) {
	t.Parallel()

	output, err := runProbe(t, "1")
	if err == nil {
		t.Fatalf("the probe passed with %s set; a suite that could not run reported "+
			"success, which is the whole defect:\n%s", requireContainers, output)
	}
	if !strings.Contains(output, "must not report success without having run") {
		t.Errorf("the failure does not say what went wrong:\n%s", output)
	}
}

// runProbe runs the probe in a subprocess with requireContainers set to required, and
// reports its combined output and whether it failed.
func runProbe(t *testing.T, required string) (string, error) {
	t.Helper()

	command := exec.Command(os.Args[0], "-test.run", "^TestNoContainerRuntimeProbe$", "-test.v")
	command.Env = append(os.Environ(), runtimeProbe+"=1", requireContainers+"="+required)
	output, err := command.CombinedOutput()
	return string(output), err
}

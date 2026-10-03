package storage_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
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

const runtimeProbe = "OC_CONTAINER_RUNTIME_PROBE"

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

func runProbe(t *testing.T, required string) (string, error) {
	t.Helper()

	command := exec.Command(os.Args[0], "-test.run", "^TestNoContainerRuntimeProbe$", "-test.v")
	command.Env = append(os.Environ(), runtimeProbe+"=1", requireContainers+"="+required)
	output, err := command.CombinedOutput()
	return string(output), err
}

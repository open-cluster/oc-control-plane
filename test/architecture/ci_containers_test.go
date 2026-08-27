package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A container runtime CI cannot reach must fail the job rather than empty it.
//
// Every composed-process test in this repository starts real containers, and every one of
// them skips when it cannot. That is right for a contributor with no runtime — the short
// suite is the supported way to work without Docker. It is wrong for CI, where a runtime
// that never came up produces a green job over a suite that never ran, and nothing in the
// output says so. An image that cannot be pulled already fails loudly; a daemon that is not
// there does not, and that asymmetry is the whole defect.
//
// RequireContainersVariable is what closes it, and this gate is here because the failure it
// prevents is invisible: a workflow that stops setting the variable goes on passing, and
// what it stops proving is exactly what nobody is watching.
const requireContainersVariable = "OC_REQUIRE_CONTAINERS"

// The workflow steps that run container-backed tests. Each is named by the run line that
// identifies it, because a step's name is prose somebody may reword while the command it
// runs stays the same thing.
var containerBackedSteps = []string{
	"run: make test",
	"run: cd test/e2e && go test",
}

func TestContinuousIntegrationRequiresAContainerRuntime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(moduleRoot, ".github", "workflows", "ci.yml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the ci workflow: %v", err)
	}
	workflow := string(content)

	for _, step := range containerBackedSteps {
		index := strings.Index(workflow, step)
		if index < 0 {
			t.Errorf("the ci workflow no longer has a step running %q; if the command "+
				"moved, this gate has to move with it, because what it protects is that "+
				"the step declares %s", step, requireContainersVariable)
			continue
		}
		if !declaresRequireContainers(workflow, index) {
			t.Errorf("the ci workflow step running %q does not declare %s; without it a "+
				"run whose container runtime never came up reports success over a suite "+
				"that never executed", step, requireContainersVariable)
		}
	}
}

// declaresRequireContainers reports whether the step ending at the run line beginning at
// index carries the variable. A step's env block precedes its run line, so the search runs
// backwards from the command to the start of the step — the previous "- name:" — which is
// what keeps a variable set on a NEIGHBOURING step from satisfying this.
func declaresRequireContainers(workflow string, index int) bool {
	step := workflow[:index]
	if start := strings.LastIndex(step, "- name:"); start >= 0 {
		step = step[start:]
	}
	return strings.Contains(step, requireContainersVariable)
}

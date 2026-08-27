package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// envRelaySource names the Relay's working tree. It is configuration rather than a constant
// because the two repositories share no history and nothing is copied between them, so the
// Relay's source is wherever the person or the CI job running this put it.
const envRelaySource = "OC_E2E_RELAY_SOURCE"

// buildTimeout bounds a compile. Both trees pull large dependency graphs on a cold module
// cache, and a build that hangs should say so rather than be mistaken for a slow test.
const buildTimeout = 10 * time.Minute

// buildRoot holds the compiled binaries for the whole package run; TestMain creates and
// removes it. Building once rather than per test matters: a Kubernetes dependency graph
// takes long enough that per-test builds would dominate the run.
var buildRoot string

// The two halves, each compiled at most once however many tests ask for them.
var (
	controlPlaneBinary = sync.OnceValues(func() (string, error) {
		return build("controlplane-e2e", filepath.Join(controlPlaneSource(), "test", "e2e"),
			"./cmd/controlplane-e2e")
	})
	relayBinary = sync.OnceValues(func() (string, error) {
		source, err := relaySource()
		if err != nil {
			return "", err
		}
		return build("opencluster-relay", source, "./cmd/opencluster-relay")
	})
)

// errRelaySourceMissing reports that the Relay's working tree could not be found. Tests
// treat it as a skip rather than a failure: a machine without the Relay checked out has not
// disproved anything.
var errRelaySourceMissing = errors.New("the Relay's working tree was not found")

// controlPlaneSource is this repository's root.
//
// It is FOUND rather than assumed to be two levels up, because the scenario harness is a
// program in this module as well as a test package, and a program is run from wherever the
// person running it happens to be. A relative path that only works from one directory is a
// harness that fails with a compile error instead of running.
func controlPlaneSource() string { return repositoryRoot() }

// repositoryRoot walks up from the working directory to the module that names this repository.
// Falling back to the historical relative path keeps the tests working in the one case the walk
// cannot serve — a checkout whose go.mod is unreadable — rather than turning it into a panic.
var repositoryRoot = sync.OnceValue(func() string {
	const modulePath = "module github.com/open-cluster/oc-control-plane"

	directory, err := os.Getwd()
	if err != nil {
		return filepath.Join("..", "..")
	}
	for {
		declaration, readErr := os.ReadFile(filepath.Join(directory, "go.mod"))
		if readErr == nil && strings.Contains(string(declaration), modulePath+"\n") {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return filepath.Join("..", "..")
		}
		directory = parent
	}
})

// relaySource resolves the Relay's working tree: the configured path, or the sibling
// directory that a side-by-side checkout produces.
func relaySource() (string, error) {
	configured := strings.TrimSpace(os.Getenv(envRelaySource))
	if configured != "" {
		if _, err := os.Stat(filepath.Join(configured, "go.mod")); err != nil {
			return "", fmt.Errorf("%w: %s names %q, which has no go.mod",
				errRelaySourceMissing, envRelaySource, configured)
		}
		return configured, nil
	}

	sibling := filepath.Join(repositoryRoot(), "..", "opencluster-relay")
	if _, err := os.Stat(filepath.Join(sibling, "go.mod")); err != nil {
		return "", fmt.Errorf("%w beside this repository; set %s to its path",
			errRelaySourceMissing, envRelaySource)
	}
	return sibling, nil
}

// useBuildRoot points the compiled binaries at a directory, and reports how to remove it. The
// test run sets it once in TestMain; the scenario harness sets it once at startup. Building the
// two halves per invocation rather than per caller is what keeps a Kubernetes dependency graph
// from dominating the run.
func useBuildRoot() (func(), error) {
	root, err := os.MkdirTemp("", "oc-e2e-build")
	if err != nil {
		return nil, fmt.Errorf("creating the build root: %w", err)
	}
	buildRoot = root
	return func() { _ = os.RemoveAll(root) }, nil
}

// build compiles one package of one module into the shared build root.
func build(name, moduleDir, target string) (string, error) {
	binary := filepath.Join(buildRoot, name)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	command := exec.Command("go", "build", "-o", binary, target)
	command.Dir = moduleDir
	command.WaitDelay = buildTimeout

	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building %s from %s: %w\n%s", target, moduleDir, err, output)
	}
	return binary, nil
}

// program is one half running as a real process.
//
// Its exit is recorded exactly once and then readable by anyone. Two callers want it for
// opposite reasons — a test waiting for a process to fail on its own, and cleanup killing one
// that did not — and they can arrive in either order, so neither may consume an answer the
// other still needs.
type program struct {
	name    string
	command *exec.Cmd
	output  *syncBuffer

	// exited is closed once the process has gone and exitErr has been written, which is what
	// makes exitErr safe to read: every reader synchronises on the close.
	exited  chan struct{}
	exitErr error
}

// startProgram launches binary with exactly the given environment — inherited variables are
// deliberately not passed on, so a stray OC_ or RELAY_ setting on the machine running this
// cannot change what is being proven.
//
// The exceptions are the few a process needs to run at all rather than to be configured:
// PATH, and on Windows SYSTEMROOT, without which a process cannot open a socket. The
// temporary directory is carried under both names it goes by so a child that needs scratch
// space has somewhere on either platform.
// The output buffer is supplied by the caller so it can span restarts of the same half.
func startProgram(
	name, binary string, environment map[string]string, output *syncBuffer,
) (*program, error) {
	command := exec.Command(binary)
	command.Env = environmentFor(environment)
	command.Stdout = output
	command.Stderr = output

	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", name, err)
	}

	running := &program{
		name:    name,
		command: command,
		output:  output,
		exited:  make(chan struct{}),
	}
	go func() {
		running.exitErr = command.Wait()
		close(running.exited)
	}()
	return running, nil
}

// environmentFor renders the process environment, carrying through only the few inherited
// variables a child needs to run at all.
func environmentFor(environment map[string]string) []string {
	rendered := make([]string, 0, len(environment)+4)
	for _, inherited := range []string{"PATH", "SYSTEMROOT", "TEMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(inherited); ok {
			rendered = append(rendered, inherited+"="+value)
		}
	}
	for key, value := range environment {
		rendered = append(rendered, key+"="+value)
	}
	return rendered
}

// kill stops the process abruptly and waits for it to go.
//
// Abruptly is deliberate. Delivering a termination signal portably would need
// platform-specific code on Windows, and what that code would buy is a graceful drain —
// which both halves already test on their own, where the signal can be delivered directly.
// What this harness is for is the other case: a process that dies without warning, leaving
// leases to expire and work to be recovered. Killing is that case, so nothing is lost by not
// having the polite one.
// Killing an already-dead process is harmless, so this needs no guard against being called
// twice — which matters, because cleanup calls it after tests that already waited it out.
func (p *program) kill() {
	_ = p.command.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
	}
}

// wait blocks until the process exits on its own and reports how it exited, or kills it and
// says so when the budget runs out. It is how a test asserts that a half refused to start —
// the Relay handed a spent token, for instance.
func (p *program) wait(budget time.Duration) error {
	select {
	case <-p.exited:
		return p.exitErr
	case <-time.After(budget):
		p.kill()
		return fmt.Errorf("%s was still running after %s", p.name, budget)
	}
}

// running reports whether the process is still alive, without blocking on it.
func (p *program) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// syncBuffer collects output. Both halves write from several goroutines while a test reads,
// and the output is the whole diagnostic: a failure here has two candidate causes, and the
// logs are what tells them apart.
//
// One buffer outlives the processes that write into it, so a half that is restarted keeps
// what its predecessor said. Otherwise the last thing to go wrong would be reported against a
// log that begins after everything interesting happened.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.String()
}

// mark writes a separator, so a reader can see where one process ended and the next began.
func (s *syncBuffer) mark(note string) {
	_, _ = s.Write([]byte("--- " + note + " ---\n"))
}

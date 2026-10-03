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

const envRelaySource = "OC_E2E_RELAY_SOURCE"

const buildTimeout = 10 * time.Minute

var buildRoot string

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

var errRelaySourceMissing = errors.New("the Relay's working tree was not found")

func controlPlaneSource() string { return repositoryRoot() }

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

func useBuildRoot() (func(), error) {
	root, err := os.MkdirTemp("", "oc-e2e-build")
	if err != nil {
		return nil, fmt.Errorf("creating the build root: %w", err)
	}
	buildRoot = root
	return func() { _ = os.RemoveAll(root) }, nil
}

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

type program struct {
	name    string
	command *exec.Cmd
	output  *syncBuffer

	exited  chan struct{}
	exitErr error
}

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

func (p *program) kill() {
	_ = p.command.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
	}
}

func (p *program) wait(budget time.Duration) error {
	select {
	case <-p.exited:
		return p.exitErr
	case <-time.After(budget):
		p.kill()
		return fmt.Errorf("%s was still running after %s", p.name, budget)
	}
}

func (p *program) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

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

func (s *syncBuffer) mark(note string) {
	_, _ = s.Write([]byte("--- " + note + " ---\n"))
}

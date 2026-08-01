package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// controlPlaneStartTimeout bounds how long the process may take to bind and report ready. It
// includes applying the migrations, which is the slowest thing a first start does.
const controlPlaneStartTimeout = 2 * time.Minute

// controlPlane is the control plane running as a real process.
//
// Its ports are fixed for the lifetime of the harness rather than ephemeral per start,
// because the TLS terminator in front of it and the Relay's persisted view of where to
// connect both outlive any single process. A restart that moved the port would be a property
// of the harness rather than of the control plane.
type controlPlane struct {
	program *program
	// output spans restarts, so a failure after one is still reported against everything the
	// control plane said, not only what the process that happened to be alive said.
	output *syncBuffer
	starts int

	httpAddress  string
	relayAddress string
	spkiPin      string
	dsnPath      string
	workDir      string

	// The operator surface and the model boundary, both empty for the protocol proof and both
	// required by the scenario harness. The harness drives the product the way an engineer does —
	// through the operator API — rather than by writing rows, so the surface has to be bound; and
	// the control plane runs here as a child process, so its model boundary can only arrive by
	// configuration.
	operatorAddress   string
	operatorToken     string
	operatorTokenPath string
	transcriptPath    string
}

// serveOperator binds the operator surface behind a token of this run's own. The token is written
// to a file because that is the only way the control plane accepts one — it keeps a digest and
// never the token itself, so there is nothing in the process to log by accident.
func (c *controlPlane) serveOperator(token string) error {
	port, err := reservePort()
	if err != nil {
		return err
	}
	path := filepath.Join(c.workDir, "operator.token")
	if err = os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("writing the operator token: %w", err)
	}
	c.operatorAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c.operatorToken = token
	c.operatorTokenPath = path
	return nil
}

// replayTranscript points the model boundary at a recording.
//
// It is a file rather than a provider because there is no live provider in this build. The
// harness's own documentation says so as a named gap rather than leaving a reader to infer it
// from the absence of a flag.
func (c *controlPlane) replayTranscript(path string) { c.transcriptPath = path }

// newControlPlane reserves the addresses the control plane will serve on, without starting
// it. The relay address is needed before the process exists, because the TLS terminator that
// fronts it — and therefore the pin the process must advertise — has to be built first.
func newControlPlane(workDir, dsn string) (*controlPlane, error) {
	dsnPath := filepath.Join(workDir, "placement.dsn")
	if err := os.WriteFile(dsnPath, []byte(dsn), 0o600); err != nil {
		return nil, fmt.Errorf("writing the placement dsn: %w", err)
	}

	httpPort, err := reservePort()
	if err != nil {
		return nil, err
	}
	relayPort, err := reservePort()
	if err != nil {
		return nil, err
	}
	return &controlPlane{
		output:       &syncBuffer{},
		httpAddress:  net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)),
		relayAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort)),
		dsnPath:      dsnPath,
		workDir:      workDir,
	}, nil
}

// start launches the process and waits for it to report ready. spkiPin is the key of the
// terminator in front of it: the control plane hands this to a relay at enrolment, and the
// relay pins every later connection to it, so a mismatch here would show up as an
// unexplainable handshake refusal rather than as configuration.
func (c *controlPlane) start(ctx context.Context, spkiPin string) error {
	binary, err := controlPlaneBinary()
	if err != nil {
		return err
	}
	c.spkiPin = spkiPin
	c.starts++
	c.output.mark(fmt.Sprintf("control plane, start %d", c.starts))

	environment := map[string]string{
		"OC_HTTP_ADDRESS":      c.httpAddress,
		"OC_PLACEMENTS":        "shared=" + c.dsnPath,
		"OC_DEFAULT_PLACEMENT": "shared",
		"OC_RELAY_ADDRESS":     c.relayAddress,
		"OC_RELAY_SPKI_PINS":   spkiPin,
		"OC_SHUTDOWN_TIMEOUT":  "10s",
		"OC_SERVICE_NAME":      "oc-control-plane-e2e",
	}
	if c.operatorAddress != "" {
		environment["OC_OPERATOR_ADDRESS"] = c.operatorAddress
		environment["OC_OPERATOR_TOKEN_FILE"] = c.operatorTokenPath
	}
	if c.transcriptPath != "" {
		environment["OC_MODEL_TRANSCRIPT_FILE"] = c.transcriptPath
	}

	running, err := startProgram("control plane", binary, environment, c.output)
	if err != nil {
		return err
	}
	c.program = running
	return c.awaitReady(ctx)
}

// restart kills the process and brings another up on the same addresses. It models the
// control plane going away without warning — a crash, an evicted pod, a node lost — which is
// the case the durable job model exists to survive.
func (c *controlPlane) restart(ctx context.Context) error {
	c.program.kill()
	return c.start(ctx, c.spkiPin)
}

func (c *controlPlane) stop() {
	if c == nil || c.program == nil {
		return
	}
	c.program.kill()
}

func (c *controlPlane) logs() string {
	if c == nil {
		return ""
	}
	return c.output.String()
}

// logsSinceStart is what the process running now has said, which is what a claim about a
// restarted control plane has to rest on. Searching the whole history instead would find its
// predecessor's line and call it proof.
func (c *controlPlane) logsSinceStart() string {
	whole := c.logs()
	marker := fmt.Sprintf("--- control plane, start %d ---", c.starts)
	if index := strings.LastIndex(whole, marker); index >= 0 {
		return whole[index:]
	}
	return whole
}

// awaitReady polls readiness until the process answers or gives up. Readiness rather than a
// log line, because readiness is the statement that the database is reachable — and every
// assertion in this harness is a read of that database.
func (c *controlPlane) awaitReady(ctx context.Context) error {
	deadline := time.Now().Add(controlPlaneStartTimeout)
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		if !c.program.running() {
			return fmt.Errorf("the control plane exited before it was ready\n%s", c.logs())
		}
		if ready(ctx, client, "http://"+c.httpAddress+"/readyz") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the control plane was not ready within %s\n%s",
				controlPlaneStartTimeout, c.logs())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ready(ctx context.Context, client *http.Client, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

// reservePort finds a port nothing is listening on and releases it again.
//
// There is a window between releasing and binding in which something else could take it. It
// is accepted rather than closed because the alternative — handing an already-bound listener
// to a child process — is platform-specific work for a race that has one loser: a start that
// fails loudly with an address already in use.
func reservePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserving a port: %w", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

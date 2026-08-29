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
const (
	controlPlaneStartTimeout   = 2 * time.Minute
	organization               = "local"
	investigationOperatorToken = "e2e-investigation-operator-token-with-sufficient-entropy"
)

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
	operatorPath string
	modelKeyPath string
	sealingPath  string
	modelURL     string
	workDir      string
	session      *http.Cookie
}

// newControlPlane reserves the addresses the control plane will serve on, without starting
// it. The relay address is needed before the process exists, because the TLS terminator that
// fronts it — and therefore the pin the process must advertise — has to be built first.
func newControlPlane(workDir, dsn, modelURL string) (*controlPlane, error) {
	dsnPath := filepath.Join(workDir, "database.dsn")
	if err := os.WriteFile(dsnPath, []byte(dsn), 0o600); err != nil {
		return nil, fmt.Errorf("writing the database dsn: %w", err)
	}
	operatorPath := filepath.Join(workDir, "operator.token")
	if err := os.WriteFile(operatorPath, []byte(investigationOperatorToken), 0o600); err != nil {
		return nil, fmt.Errorf("writing the operator token: %w", err)
	}
	modelKeyPath := filepath.Join(workDir, "model.key")
	if err := os.WriteFile(modelKeyPath, []byte("e2e-scripted-model-credential"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the model credential: %w", err)
	}
	sealingPath := filepath.Join(workDir, "sealing.key")
	if err := os.WriteFile(sealingPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the investigation sealing key: %w", err)
	}

	httpPort, relayPort, err := reservePorts()
	if err != nil {
		return nil, err
	}
	return &controlPlane{
		output:       &syncBuffer{},
		httpAddress:  net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)),
		relayAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort)),
		dsnPath:      dsnPath,
		operatorPath: operatorPath,
		modelKeyPath: modelKeyPath,
		sealingPath:  sealingPath,
		modelURL:     modelURL,
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
		"OC_SERVER_ADDRESS":       c.httpAddress,
		"OC_PUBLIC_URL":           "http://" + c.httpAddress,
		"OC_DATABASE_DSN_FILE":    c.dsnPath,
		"OC_RELAY_ADDRESS":        c.relayAddress,
		"OC_RELAY_SPKI_PINS":      spkiPin,
		"OC_BOOTSTRAP_TOKEN_FILE": c.operatorPath,
		"OC_AI_PROVIDER":          "anthropic",
		"OC_AI_MODEL":             "claude-sonnet-5",
		"OC_AI_API_KEY_FILE":      c.modelKeyPath,
		"OC_ENCRYPTION_KEY_FILE":  c.sealingPath,
		"OC_E2E_MODEL_BASE_URL":   c.modelURL,
	}

	running, err := startProgram("control plane", binary, environment, c.output)
	if err != nil {
		return err
	}
	c.program = running
	if err := c.awaitReady(ctx); err != nil {
		return err
	}
	if c.session == nil {
		return c.bootstrap(ctx)
	}
	return nil
}

func (c *controlPlane) bootstrap(ctx context.Context) error {
	body := strings.NewReader(`{"email":"sre@example.test","displayName":"E2E SRE","password":"temporary e2e administrator password"}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+c.httpAddress+"/api/v1/auth/local/bootstrap", body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+investigationOperatorToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://"+c.httpAddress)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("bootstrapping the e2e administrator: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("bootstrapping the e2e administrator returned %d", response.StatusCode)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Value != "" {
			c.session = cookie
			organizationBody := strings.NewReader(`{"displayName":"E2E Organization","requestedSlug":"` + organization + `"}`)
			organizationRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost,
				"http://"+c.httpAddress+"/api/v1/organizations", organizationBody)
			if requestErr != nil {
				return requestErr
			}
			organizationRequest.Header.Set("Content-Type", "application/json")
			organizationRequest.Header.Set("Origin", "http://"+c.httpAddress)
			organizationRequest.AddCookie(c.session)
			organizationResponse, requestErr := http.DefaultClient.Do(organizationRequest)
			if requestErr != nil {
				return fmt.Errorf("creating the e2e Organization: %w", requestErr)
			}
			defer func() { _ = organizationResponse.Body.Close() }()
			if organizationResponse.StatusCode != http.StatusCreated {
				return fmt.Errorf("creating the e2e Organization returned %d",
					organizationResponse.StatusCode)
			}
			return nil
		}
	}
	return fmt.Errorf("bootstrapping the e2e administrator issued no session")
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

// reservePorts finds two distinct ports nothing is listening on and releases them together.
//
// There is a window between releasing and binding in which something else could take it. It
// is accepted rather than closed because the alternative — handing an already-bound listener
// to a child process — is platform-specific work for a race that has one loser: a start that
// fails loudly with an address already in use.
func reservePorts() (int, int, error) {
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, fmt.Errorf("reserving an HTTP port: %w", err)
	}
	defer func() { _ = httpListener.Close() }()
	relayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, fmt.Errorf("reserving a Relay port: %w", err)
	}
	defer func() { _ = relayListener.Close() }()
	return httpListener.Addr().(*net.TCPAddr).Port,
		relayListener.Addr().(*net.TCPAddr).Port, nil
}

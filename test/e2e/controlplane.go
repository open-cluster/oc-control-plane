package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	controlPlaneStartTimeout    = 2 * time.Minute
	investigationBootstrapToken = "e2e-investigation-bootstrap-token-with-sufficient-entropy"
)

var organization string

type controlPlane struct {
	program *program
	output  *syncBuffer
	starts  int

	httpAddress   string
	relayAddress  string
	spkiPin       string
	dsnPath       string
	bootstrapPath string
	modelKeyPath  string
	sealingPath   string
	modelURL      string
	workDir       string
	session       *http.Cookie
}

func newControlPlane(workDir, dsn, modelURL string) (*controlPlane, error) {
	dsnPath := filepath.Join(workDir, "database.dsn")
	if err := os.WriteFile(dsnPath, []byte(dsn), 0o600); err != nil {
		return nil, fmt.Errorf("writing the database dsn: %w", err)
	}
	bootstrapPath := filepath.Join(workDir, "bootstrap.token")
	if err := os.WriteFile(bootstrapPath, []byte(investigationBootstrapToken), 0o600); err != nil {
		return nil, fmt.Errorf("writing the bootstrap token: %w", err)
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
		output:        &syncBuffer{},
		httpAddress:   net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)),
		relayAddress:  net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort)),
		dsnPath:       dsnPath,
		bootstrapPath: bootstrapPath,
		modelKeyPath:  modelKeyPath,
		sealingPath:   sealingPath,
		modelURL:      modelURL,
		workDir:       workDir,
	}, nil
}

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
		"OC_BOOTSTRAP_TOKEN_FILE": c.bootstrapPath,
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
	body := strings.NewReader(`{"organizationName":"E2E Organization","email":"sre@example.test","displayName":"E2E SRE","password":"temporary e2e administrator password"}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+c.httpAddress+"/api/v1/auth/local/bootstrap", body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+investigationBootstrapToken)
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
			sessionRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
				"http://"+c.httpAddress+"/api/v1/session", nil)
			if requestErr != nil {
				return requestErr
			}
			sessionRequest.AddCookie(c.session)
			sessionResponse, requestErr := http.DefaultClient.Do(sessionRequest)
			if requestErr != nil {
				return fmt.Errorf("reading the e2e session: %w", requestErr)
			}
			defer func() { _ = sessionResponse.Body.Close() }()
			if sessionResponse.StatusCode != http.StatusOK {
				return fmt.Errorf("reading the e2e session returned %d", sessionResponse.StatusCode)
			}
			var session struct {
				Organization struct {
					ID string `json:"id"`
				} `json:"organization"`
			}
			if requestErr = json.NewDecoder(sessionResponse.Body).Decode(&session); requestErr != nil {
				return fmt.Errorf("decoding the e2e session: %w", requestErr)
			}
			organization = session.Organization.ID
			return nil
		}
	}
	return fmt.Errorf("bootstrapping the e2e administrator issued no session")
}

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

func (c *controlPlane) logsSinceStart() string {
	whole := c.logs()
	marker := fmt.Sprintf("--- control plane, start %d ---", c.starts)
	if index := strings.LastIndex(whole, marker); index >= 0 {
		return whole[index:]
	}
	return whole
}

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

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

// TestMain establishes goroutine-leak detection for the whole package, so the discipline
// exists before anything concurrent depends on it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// Test-harness background workers that outlive individual tests. Each belongs to the
		// Docker client or the database driver, never to the control plane itself — the point
		// of this check is that OUR goroutines do not outlive the process context.
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		// Windows-only: the Docker client's named-pipe I/O completion processor.
		goleak.IgnoreAnyFunction("github.com/Microsoft/go-winio.ioCompletionProcessor"),
		// Testcontainers' reaper, which watches the container this package shares. It outlives the
		// tests deliberately: the container is not torn down per test, so the connection that
		// guards it must not be either.
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
	)
}

func selectOrganizationFromURL(request *http.Request) {
	const prefix = "/api/v1/organizations/"
	remainder, found := strings.CutPrefix(request.URL.Path, prefix)
	if !found {
		return
	}
	organization, resource, _ := strings.Cut(remainder, "/")
	if organization != "" {
		request.Header.Set(authz.OrganizationHeader, organization)
		request.URL.Path = "/api/v1"
		if resource != "" {
			request.URL.Path += "/" + resource
		}
	}
}

// controlPlane starts one Postgres, runs the composition root against it, and returns the
// base URL plus the captured log output. This is the single behavioural seam: the assembled
// process, a real database, a real listener, real alertEvents.
type controlPlane struct {
	baseURL       string
	sessionCookie string
	logs          *syncBuffer
	stop          context.CancelFunc
	exited        chan error
	// waitOnce and exitErr let shutdown be called twice — once by a test that wants to observe
	// the exit, once by cleanup — without the second call waiting out its timeout on a channel
	// the first already drained.
	waitOnce sync.Once
	exitErr  error
	// database is a TCP gate in front of Postgres. Closing it models a database outage at
	// a STABLE address, which is what a real one looks like: a service endpoint that stops
	// answering and later answers again. Stopping the container instead would move the
	// mapped port, so recovery could never be observed — an artefact of the harness rather
	// than a property of the control plane.
	database *tcpGate
}

// gateMode is how the gate treats connections. The three model the ways a database becomes
// unusable: reachable, refusing, and accepting-but-never-answering.
type gateMode int

const (
	gateOpen gateMode = iota
	gateClosed
	gateBlackhole
)

// tcpGate forwards connections to a target and can be closed and reopened without changing
// its own address.
type tcpGate struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	mode   gateMode
	closed bool
	// live tracks established connections so closing the gate can drop them. Refusing only
	// NEW connections would leave a pooled connection working, and readiness would keep
	// reporting ready through a simulated outage.
	live map[net.Conn]struct{}

	active sync.WaitGroup
	done   chan struct{}
}

func newTCPGate(t *testing.T, target string) *tcpGate {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gate listen: %v", err)
	}

	gate := &tcpGate{
		listener: listener,
		target:   target,
		mode:     gateOpen,
		live:     make(map[net.Conn]struct{}),
		done:     make(chan struct{}),
	}
	go gate.accept()
	t.Cleanup(gate.shutdown)
	return gate
}

// track registers a connection for the lifetime of its forwarding, so closeGate can drop
// it. It returns false when the gate closed between accept and registration.
func (g *tcpGate) track(connections ...net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != gateOpen {
		return false
	}
	for _, connection := range connections {
		g.live[connection] = struct{}{}
	}
	return true
}

func (g *tcpGate) untrack(connections ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, connection := range connections {
		delete(g.live, connection)
	}
}

func (g *tcpGate) address() string { return g.listener.Addr().String() }

func (g *tcpGate) accept() {
	for {
		connection, err := g.listener.Accept()
		if err != nil {
			return
		}

		g.mu.Lock()
		mode := g.mode
		g.mu.Unlock()

		switch mode {
		case gateClosed:
			_ = connection.Close()
			continue
		case gateBlackhole:
			// Accept and never answer. The connection is held until the gate shuts down.
			g.active.Add(1)
			go func() {
				defer g.active.Done()
				defer func() { _ = connection.Close() }()
				<-g.done
			}()
			continue
		}

		g.active.Add(1)
		go func() {
			defer g.active.Done()
			g.forward(connection)
		}()
	}
}

func (g *tcpGate) forward(client net.Conn) {
	defer func() { _ = client.Close() }()

	upstream, err := net.DialTimeout("tcp", g.target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	if !g.track(client, upstream) {
		return
	}
	defer g.untrack(client, upstream)

	finished := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); finished <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); finished <- struct{}{} }()

	select {
	case <-finished:
	case <-g.done:
	}
}

// closeGate refuses new connections AND drops established ones, so pooled connections
// break exactly as they would in a real outage. Refusing only new connections would leave
// an idle pooled connection working and readiness reporting ready throughout.
func (g *tcpGate) closeGate() { g.setMode(gateClosed) }

// blackhole makes the database accept connections and never answer, so a caller blocks
// until its own deadline instead of failing fast.
func (g *tcpGate) blackhole() { g.setMode(gateBlackhole) }

func (g *tcpGate) openGate() { g.setMode(gateOpen) }

// setMode switches the gate and drops every established connection, so a pooled connection
// cannot keep working through a simulated outage.
func (g *tcpGate) setMode(mode gateMode) {
	g.mu.Lock()
	g.mode = mode
	dropping := make([]net.Conn, 0, len(g.live))
	for connection := range g.live {
		dropping = append(dropping, connection)
	}
	g.live = make(map[net.Conn]struct{})
	g.mu.Unlock()

	for _, connection := range dropping {
		_ = connection.Close()
	}
}

func (g *tcpGate) shutdown() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.mu.Unlock()

	close(g.done)
	_ = g.listener.Close()
	g.active.Wait()
}

// postgresServer starts one Postgres for the whole package, once.
//
// It is deliberately not terminated by a cleanup: it outlives every test that uses it, and
// Testcontainers' reaper removes it when the process ends. A per-test container would be torn down
// while another test was still using it once anything here runs in parallel.
var postgresServer = sync.OnceValues(func() (string, error) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("controlplane"),
		tcpostgres.WithUsername("controlplane"),
		tcpostgres.WithPassword("controlplane"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return "", err
	}
	return container.ConnectionString(ctx, "sslmode=disable")
})

// databases numbers the databases this package creates, so two planes never share one.
var databases atomic.Int64

// freshDatabase returns a DSN for an empty database on the shared server.
func freshDatabase(t *testing.T) string {
	t.Helper()

	admin, err := postgresServer()
	if err != nil {
		noContainerRuntime(t, err)
	}
	return createDatabase(t, admin, "plane"+strconv.FormatInt(databases.Add(1), 10))
}

func startControlPlane(t *testing.T, adjust func(*config.Config)) *controlPlane {
	t.Helper()
	return startControlPlaneRunning(t, adjust, app.Options{})
}

// startControlPlaneRunning is the whole harness: the assembled process, a real database, a real
// listener, real alertEvents, and whatever the test puts in place of the model boundary.
func startControlPlaneRunning(
	t *testing.T, adjust func(*config.Config), options app.Options,
) *controlPlane {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	// One server for the package, a fresh DATABASE per plane. Starting a container per test cost
	// more wall clock than every test in this package spends doing its actual work, and it put the
	// package past the default test timeout as the suite grew. Each plane still gets an empty
	// schema, which is the isolation these tests actually depend on.
	dsn := freshDatabase(t)

	upstream, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	gate := newTCPGate(t, net.JoinHostPort(upstream.Host, strconv.Itoa(int(upstream.Port))))
	gatedDSN := "postgres://" + upstream.User + ":" + upstream.Password +
		"@" + gate.address() + "/" + upstream.Database + "?sslmode=disable"

	cfg := config.Config{
		HTTPAddress: freeAddress(t),
		DatabaseDSN: gatedDSN,
		// A default sealing key, because the catalog serves a credential-bearing type and
		// an operator surface without a key refuses to start. A test proving that refusal
		// clears this deliberately.
		SealingKey: bytes.Repeat([]byte{7}, 32),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	if cfg.OperatorPublicURL == "" {
		cfg.OperatorPublicURL = "http://" + cfg.HTTPAddress
	}

	runCtx, stop := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	addresses := make(chan net.Addr, 1)
	exited := make(chan error, 1)

	go func() {
		options.OnListen = func(addr net.Addr) { addresses <- addr }
		exited <- app.Run(runCtx, cfg, logs, options)
	}()

	var address net.Addr
	select {
	case address = <-addresses:
	case err := <-exited:
		t.Fatalf("the control plane exited before listening: %v\nlogs:\n%s", err, logs.String())
	case <-time.After(90 * time.Second):
		t.Fatalf("the control plane did not listen in time\nlogs:\n%s", logs.String())
	}

	plane := &controlPlane{
		baseURL:  "http://" + address.String(),
		logs:     logs,
		stop:     stop,
		exited:   exited,
		database: gate,
	}
	t.Cleanup(plane.shutdown)
	surfaceDigest := sha256.Sum256([]byte(surfaceToken))
	if bytes.Equal(cfg.OperatorTokenDigest, surfaceDigest[:]) {
		plane.bootstrapAdmin(t, cfg.OperatorTokenOrganization, surfaceToken,
			cfg.OperatorPublicURL)
	}
	return plane
}

func (c *controlPlane) bootstrapAdmin(
	t *testing.T, organization, token, origin string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"email": "admin@example.test", "displayName": "Test Administrator",
		"password": "temporary integration test administrator password",
	})
	if err != nil {
		t.Fatalf("encode bootstrap request: %v", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		c.baseURL+"/api/v1/auth/local/bootstrap", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("bootstrap integration-test administrator: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("bootstrap integration-test administrator = %d: %s",
			response.StatusCode, raw)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == session.CookieName && cookie.Value != "" {
			c.sessionCookie = cookie.Value
			organizationBody, marshalErr := json.Marshal(map[string]string{
				"displayName": "Test Organization", "requestedSlug": organization,
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			organizationRequest, requestErr := http.NewRequestWithContext(
				context.Background(), http.MethodPost, c.baseURL+"/api/v1/organizations",
				bytes.NewReader(organizationBody))
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			organizationRequest.Header.Set("Content-Type", "application/json")
			organizationRequest.Header.Set("Origin", origin)
			organizationRequest.AddCookie(&http.Cookie{Name: session.CookieName, Value: c.sessionCookie})
			organizationResponse, requestErr := http.DefaultClient.Do(organizationRequest)
			if requestErr != nil {
				t.Fatalf("create integration-test Organization: %v", requestErr)
			}
			defer func() { _ = organizationResponse.Body.Close() }()
			if organizationResponse.StatusCode != http.StatusCreated {
				organizationRaw, _ := io.ReadAll(organizationResponse.Body)
				t.Fatalf("create integration-test Organization = %d: %s",
					organizationResponse.StatusCode, organizationRaw)
			}
			return
		}
	}
	t.Fatal("bootstrap integration-test administrator issued no session cookie")
}

// shutdown cancels the process context and waits for a clean exit, recording what the exit
// was. Safe to call twice; the second call returns the first call's answer.
func (c *controlPlane) shutdown() {
	c.stop()
	c.waitOnce.Do(func() {
		select {
		case c.exitErr = <-c.exited:
		case <-time.After(30 * time.Second):
			c.exitErr = errors.New("the control plane did not stop")
		}
	})
}

func (c *controlPlane) get(t *testing.T, path string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response.StatusCode, string(body)
}

// createDatabase creates another database on the same server and returns its DSN, so one
// container can back several independent database.
func createDatabase(t *testing.T, adminDSN, name string) string {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	if _, err := connection.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	parsed, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return "postgres://" + parsed.User + ":" + parsed.Password +
		"@" + net.JoinHostPort(parsed.Host, strconv.Itoa(int(parsed.Port))) +
		"/" + name + "?sslmode=disable"
}

// syncBuffer is a concurrency-safe log sink; the process writes from several goroutines
// while the test reads.
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

// logLines parses the captured JSON log output.
func (s *syncBuffer) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(s.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestControlPlane_StartsAppliesMigrationsAndServes(t *testing.T) {
	plane := startControlPlane(t, nil)

	status, body := plane.get(t, "/healthz")
	if status != http.StatusOK {
		t.Errorf("GET /healthz = %d, body %s", status, body)
	}

	status, body = plane.get(t, "/readyz")
	if status != http.StatusOK {
		t.Errorf("GET /readyz = %d, body %s", status, body)
	}

	// The schema effect of this start must be visible without querying the database.
	if !strings.Contains(plane.logs.String(), "migrations applied") {
		t.Errorf("startup must report the migrations it applied\nlogs:\n%s", plane.logs.String())
	}
}

func TestControlPlane_ServesEveryHTTPRouteGroupOnOneAddress(t *testing.T) {
	plane := startControlPlane(t, nil)

	if status, _ := plane.get(t, "/healthz"); status != http.StatusOK {
		t.Errorf("GET /healthz = %d", status)
	}
	if status, _ := plane.get(t, "/api/v1/session"); status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/session = %d, want authentication refusal from the operator router", status)
	}
	if status, body := plane.get(t, "/"); status != http.StatusOK ||
		!strings.Contains(body, "OpenCluster Control Plane") {
		t.Errorf("GET browser application = %d, body %q", status, body)
	}
	if status, body := plane.get(t, "/organizations/local/investigations/example/sources"); status != http.StatusOK || !strings.Contains(body, "OpenCluster Control Plane") {
		t.Errorf("GET browser deep link = %d, body %q", status, body)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		plane.baseURL+"/webhooks/v1/integrations/not-an-id/alert-events", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST intake route: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		t.Errorf("POST intake route = %d, want the intake router to handle it", response.StatusCode)
	}
}

func TestControlPlane_DoesNotServePreReleaseRouteAliases(t *testing.T) {
	plane := startControlPlane(t, nil)
	for _, path := range []string{
		"/operator/v1/session",
		"/intake/v1/integrations/example/signals",
	} {
		if status, _ := plane.get(t, path); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, status)
		}
	}
}

// Liveness must report process health only. A liveness probe that consults the database
// turns a dependency outage into a crash loop.
func TestControlPlane_LivenessIgnoresDependencies(t *testing.T) {
	plane := startControlPlane(t, nil)

	plane.database.closeGate()

	status, body := plane.get(t, "/healthz")
	if status != http.StatusOK {
		t.Errorf("liveness must stay healthy while the database is down: %d %s", status, body)
	}

	// And readiness must disagree, or liveness is not actually ignoring dependencies.
	if status, _ := plane.get(t, "/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("readiness must be unready while liveness is healthy, got %d", status)
	}
}

// Readiness must fail while the database is unreachable and recover on its own once it
// returns, with no restart.
func TestControlPlane_ReadinessFailsThenRecoversWithoutRestart(t *testing.T) {
	plane := startControlPlane(t, nil)

	if status, _ := plane.get(t, "/readyz"); status != http.StatusOK {
		t.Fatalf("readiness must start ready, got %d", status)
	}

	plane.database.closeGate()
	if status, body := plane.get(t, "/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("readiness with the database down = %d %s, want 503", status, body)
	}

	plane.database.openGate()

	deadline := time.Now().Add(60 * time.Second)
	for {
		status, _ := plane.get(t, "/readyz")
		if status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness never recovered after the database returned")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Cancelling the process context must drain in-flight work and return cleanly, which is
// what makes a rolling deployment safe.
func TestControlPlane_ShutdownDrainsAndExitsCleanly(t *testing.T) {
	plane := startControlPlane(t, nil)

	if status, _ := plane.get(t, "/healthz"); status != http.StatusOK {
		t.Fatal("the plane must serve before shutdown is meaningful")
	}

	plane.stop()
	select {
	case err := <-plane.exited:
		if err != nil {
			t.Fatalf("a cancelled process context must exit cleanly, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the control plane did not exit within the drain budget")
	}

	if !strings.Contains(plane.logs.String(), `"msg":"stopped"`) {
		t.Errorf("a clean shutdown must be reported\nlogs:\n%s", plane.logs.String())
	}
}

// Draining means in-flight work FINISHES, not that the listener closes promptly. Shutting
// down with nothing in flight proves neither. A request already being served must complete
// and its response must reach the client.
func TestControlPlane_ShutdownCompletesAnInFlightRequest(t *testing.T) {
	plane := startControlPlane(t, nil)

	// Make readiness genuinely slow: the database accepts and never answers, so the handler
	// blocks inside its own readiness timeout. With a fast handler the request would finish
	// before shutdown began and the test would prove nothing.
	plane.database.blackhole()

	host := strings.TrimPrefix(plane.baseURL, "http://")
	connection, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = connection.Close() }()

	if _, err := connection.Write([]byte(
		"GET /readyz HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Give the server time to accept and dispatch, so the request is genuinely in flight
	// when the drain starts rather than still sitting in the accept queue.
	time.Sleep(500 * time.Millisecond)
	plane.stop()

	if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	response, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("an in-flight request must complete during the drain: %v", err)
	}
	// The status is 503 — readiness legitimately fails against a hung database. What matters
	// is that a complete HTTP response arrived rather than the connection being severed
	// mid-flight, which is the difference between draining and dropping.
	if !strings.Contains(string(response), "HTTP/1.1 503") {
		t.Errorf("the in-flight request must be answered, got:\n%s", string(response))
	}

	select {
	case err := <-plane.exited:
		if err != nil {
			t.Fatalf("exit after drain: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the control plane did not exit after draining")
	}
}

// Every log line for a request must carry the same identifier, and the client must be able
// to see it, or an on-call engineer cannot connect a report to a log.
func TestControlPlane_RequestsAreCorrelated(t *testing.T) {
	plane := startControlPlane(t, nil)

	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, plane.baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// A client-supplied identifier must not be trusted: it would let a caller collide two
	// unrelated requests in the logs.
	request.Header.Set(correlation.Header, "attacker-supplied")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	requestID := response.Header.Get(correlation.Header)
	if requestID == "" {
		t.Fatal("the response must carry a request identifier")
	}
	if requestID == "attacker-supplied" {
		t.Fatal("a client-supplied request identifier must not be echoed back")
	}

	var found bool
	for _, entry := range plane.logs.logLines(t) {
		if entry["request_id"] == requestID && entry["msg"] == "request served" {
			found = true
			if entry["status"] != float64(http.StatusOK) {
				t.Errorf("logged status = %v", entry["status"])
			}
			// A span context exists whether or not it is sampled, so the trace identifier
			// is available for correlation even with no collector configured. Without this
			// a log line cannot be tied to its trace.
			traceID, present := entry["trace_id"].(string)
			if !present || traceID == "" {
				t.Errorf("the request log line must carry trace_id, got %v", entry["trace_id"])
			}
			if strings.Trim(traceID, "0") == "" {
				t.Errorf("trace_id must not be the all-zero identifier, got %q", traceID)
			}
		}
	}
	if !found {
		t.Errorf("no log line carried request_id %q\nlogs:\n%s", requestID, plane.logs.String())
	}
}

// Metrics must be scrapeable, and must not carry a per-organization label: at the stated
// scale of five thousand organizations that is a cardinality failure in any
// Prometheus-shaped backend.
func TestControlPlane_MetricsAreScrapeableAndLowCardinality(t *testing.T) {
	plane := startControlPlane(t, nil)

	// Generate some traffic so there is something to report.
	for range 3 {
		plane.get(t, "/healthz")
	}

	status, body := plane.get(t, "/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d", status)
	}
	if !strings.Contains(body, "# HELP") || !strings.Contains(body, "# TYPE") {
		t.Errorf("metrics must be served in the Prometheus exposition format, got:\n%s", body)
	}
	if strings.Contains(body, "opencluster_organization") || strings.Contains(body, "organization=") {
		t.Errorf("metrics must not carry an organization label:\n%s", body)
	}
}

// Scraping must not produce a trace per scrape; a fifteen-second scrape interval would
// otherwise generate spans forever at real cost.
func TestControlPlane_MetricsScrapeIsNotTraced(t *testing.T) {
	plane := startControlPlane(t, nil)

	plane.get(t, "/metrics")

	for _, entry := range plane.logs.logLines(t) {
		if entry["msg"] == "request served" && entry["path"] == "/metrics" {
			t.Error("the metrics scrape must not go through the request-logging middleware")
		}
	}
}

// A configuration the process cannot honour must fail rather than start degraded.
func TestRun_RefusesAnUnusableListenAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}
	plane := startControlPlane(t, nil)

	// Bind the same address twice: the second attempt must fail rather than start silently
	// serving nothing.
	occupied := strings.TrimPrefix(plane.baseURL, "http://")
	cfg := config.Config{
		HTTPAddress: occupied,
		DatabaseDSN: "postgres://u:p@127.0.0.1:1/db?sslmode=disable",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := app.Run(ctx, cfg, io.Discard, app.Options{})
	if err == nil {
		t.Fatal("binding an occupied address must fail")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the failure must be reported, not deferred to a timeout")
	}
}

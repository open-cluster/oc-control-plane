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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("github.com/Microsoft/go-winio.ioCompletionProcessor"),
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
	)
}

type controlPlane struct {
	baseURL       string
	sessionCookie string
	logs          *syncBuffer
	stop          context.CancelFunc
	exited        chan error
	waitOnce      sync.Once
	exitErr       error
	database      *tcpGate
}

type gateMode int

const (
	gateOpen gateMode = iota
	gateClosed
	gateBlackhole
)

type tcpGate struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	mode   gateMode
	closed bool
	live   map[net.Conn]struct{}

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

func (g *tcpGate) closeGate() { g.setMode(gateClosed) }

func (g *tcpGate) blackhole() { g.setMode(gateBlackhole) }

func (g *tcpGate) openGate() { g.setMode(gateOpen) }

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

var databases atomic.Int64

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

func startControlPlaneRunning(
	t *testing.T, adjust func(*config.Config), options app.Options,
) *controlPlane {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}

	dsn := freshDatabase(t)

	upstream, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	gate := newTCPGate(t, net.JoinHostPort(upstream.Host, strconv.Itoa(int(upstream.Port))))
	gatedDSN := "postgres://" + upstream.User + ":" + upstream.Password +
		"@" + gate.address() + "/" + upstream.Database + "?sslmode=disable"

	cfg := config.Config{
		HTTPListenAddress: freeAddress(t),
		DatabaseDSN:       gatedDSN,
		SessionLifetime:   12 * time.Hour,
		SealingKey:        bytes.Repeat([]byte{7}, 32),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://" + cfg.HTTPListenAddress
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
	if bytes.Equal(cfg.BootstrapTokenDigest, surfaceDigest[:]) {
		plane.bootstrapAdmin(t, surfaceOrg, surfaceToken,
			cfg.PublicURL, cfg.DatabaseDSN)
	}
	return plane
}

func (c *controlPlane) bootstrapAdmin(
	t *testing.T, organization, token, origin, dsn string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"organizationName": "Operations",
		"email":            "admin@example.test", "displayName": "Test Administrator",
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
			seedTestOrganization(t, dsn, organization, "admin@example.test")
			return
		}
	}
	t.Fatal("bootstrap integration-test administrator issued no session cookie")
}

func seedTestOrganization(t *testing.T, dsn, organization, email string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	transaction, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	var userID uuid.UUID
	if err = transaction.QueryRow(ctx, `SELECT user_id FROM app_user WHERE email=$1`, email).
		Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var current string
	err = transaction.QueryRow(ctx, `SELECT org_id FROM organization_membership WHERE user_id=$1`, userID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err = transaction.Exec(ctx, `INSERT INTO organization (org_id,display_name,created_by)
			VALUES ($1,'Operations',$2)`, organization, userID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err = transaction.Exec(ctx, `INSERT INTO organization_membership
			(org_id,user_id,role) VALUES ($1,$2,'admin')`, organization, userID); err != nil {
			t.Fatal(err)
		}
	case current == organization:
		if _, err = transaction.Exec(ctx, `UPDATE organization SET display_name='Operations' WHERE org_id=$1`, organization); err != nil {
			t.Fatal(err)
		}
	default:
		if _, err = transaction.Exec(ctx, `INSERT INTO organization (org_id,display_name,created_by)
			VALUES ($1,'Operations',$2)`, organization, userID); err != nil {
			t.Fatal(err)
		}
		if _, err = transaction.Exec(ctx, `UPDATE organization_membership SET org_id=$1 WHERE user_id=$2`, organization, userID); err != nil {
			t.Fatal(err)
		}
		if _, err = transaction.Exec(ctx, `DELETE FROM organization WHERE org_id=$1`, current); err != nil {
			t.Fatal(err)
		}
	}
	if err = transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func ensureTestOrganization(t *testing.T, dsn, organization string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `INSERT INTO organization (org_id,display_name,created_by)
		VALUES ($1,'Test Organization','test') ON CONFLICT (org_id) DO NOTHING`, organization); err != nil {
		t.Fatal(err)
	}
}

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
		t.Errorf("GET /api/v1/session = %d, want authentication refusal from the API router", status)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		plane.baseURL+"/webhooks/v1/integrations/not-an-id/alert-events", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST webhook route: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		t.Errorf("POST webhook route = %d, want the webhook router to handle it", response.StatusCode)
	}
}

func TestControlPlane_TreatsRemovedAndUnknownRoutesTheSame(t *testing.T) {
	plane := startControlPlane(t, nil)
	for _, path := range []string{
		"/api/v1/meta",
		"/operator/v1/session",
		"/intake/v1/integrations/example/signals",
	} {
		if status, _ := plane.get(t, path); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, status)
		}
	}
}

func TestControlPlane_LivenessIgnoresDependencies(t *testing.T) {
	plane := startControlPlane(t, nil)

	plane.database.closeGate()

	status, body := plane.get(t, "/healthz")
	if status != http.StatusOK {
		t.Errorf("liveness must stay healthy while the database is down: %d %s", status, body)
	}

	if status, _ := plane.get(t, "/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("readiness must be unready while liveness is healthy, got %d", status)
	}
}

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

func TestControlPlane_ShutdownCompletesAnInFlightRequest(t *testing.T) {
	plane := startControlPlane(t, nil)

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

	time.Sleep(500 * time.Millisecond)
	plane.stop()

	if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	response, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("an in-flight request must complete during the drain: %v", err)
	}
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

func TestControlPlane_RequestsAreCorrelated(t *testing.T) {
	plane := startControlPlane(t, nil)

	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, plane.baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
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

func TestControlPlane_UnmatchedWebhookRequestsAreCorrelated(t *testing.T) {
	plane := startControlPlane(t, nil)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		plane.baseURL+"/webhooks/unknown", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(correlation.Header, "attacker-supplied")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unmatched webhook status = %d", response.StatusCode)
	}
	requestID := response.Header.Get(correlation.Header)
	if len(requestID) != 32 || requestID == "attacker-supplied" {
		t.Fatalf("unmatched webhook request ID = %q", requestID)
	}
	var correlated bool
	for _, entry := range plane.logs.logLines(t) {
		if entry["msg"] == "request served" && entry["request_id"] == requestID {
			correlated = true
			if entry["status"] != float64(http.StatusNotFound) {
				t.Errorf("logged unmatched webhook status = %v", entry["status"])
			}
			if traceID, ok := entry["trace_id"].(string); !ok || strings.Trim(traceID, "0") == "" {
				t.Errorf("unmatched webhook log has invalid trace_id %v", entry["trace_id"])
			}
		}
	}
	if !correlated {
		t.Errorf("no webhook log carried request_id %q\nlogs:\n%s", requestID, plane.logs.String())
	}

	limited := false
	for range 2_000 {
		status, _ := plane.get(t, "/webhooks/unknown")
		if status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("webhook surface did not enforce its bounded burst")
	}
	if status, body := plane.get(t, "/healthz"); status != http.StatusOK {
		t.Fatalf("webhook exhaustion affected health: %d %s", status, body)
	}
}

func TestControlPlane_MetricsAreScrapeableAndLowCardinality(t *testing.T) {
	plane := startControlPlane(t, nil)

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

func TestControlPlane_MetricsScrapeIsNotTraced(t *testing.T) {
	plane := startControlPlane(t, nil)

	plane.get(t, "/metrics")

	for _, entry := range plane.logs.logLines(t) {
		if entry["msg"] == "request served" && entry["path"] == "/metrics" {
			t.Error("the metrics scrape must not go through the request-logging middleware")
		}
	}
}

func TestRun_RefusesAnUnusableListenAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: requires a Docker daemon")
	}
	plane := startControlPlane(t, nil)

	occupied := strings.TrimPrefix(plane.baseURL, "http://")
	cfg := config.Config{
		HTTPListenAddress: occupied,
		DatabaseDSN:       "postgres://u:p@127.0.0.1:1/db?sslmode=disable",
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

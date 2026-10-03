package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
	modelagent "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

const (
	alertmanagerGateImage = "prom/alertmanager:v0.34.0"
)

func TestAlertmanagerGate_PinnedNotifierRetryContract(t *testing.T) {
	if alertmanagerGateImage != "prom/alertmanager:v0.34.0" {
		t.Fatalf("the supported Alertmanager changed to %s; re-check its webhook retry contract",
			alertmanagerGateImage)
	}
	if alertmanagerWebhookRetries(http.StatusTooManyRequests) {
		t.Fatal("the pinned Alertmanager contract must treat 429 as non-retryable")
	}
	if !alertmanagerWebhookRetries(http.StatusServiceUnavailable) {
		t.Fatal("the pinned Alertmanager contract must treat 503 as retryable")
	}
}

func alertmanagerWebhookRetries(status int) bool {
	return status/100 == 5
}

type alertmanagerGate struct {
	*integrationPlane
	integration  string
	secret       string
	recorder     *intakeRecorder
	alertmanager string
	prompts      <-chan modelagent.Prompt
}

func startAlertmanagerGate(t *testing.T) *alertmanagerGate {
	t.Helper()

	apiAddress := freeAddress(t)
	intakeAddress := apiAddress
	var dsn string
	prompts := make(chan modelagent.Prompt, 1)
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = apiAddress
		cfg.HTTPListenAddress = intakeAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		cfg.ModelProvider = "zai"
		cfg.ModelName = "glm-4.7"
		cfg.ModelAPIKey = "scripted-model-key"
		dsn = cfg.DatabaseDSN
	}, app.Options{Completer: concludingModel{prompts: prompts}})

	surface := &integrationPlane{
		controlPlane: plane, api: apiAddress, intake: intakeAddress, dsn: dsn,
	}
	created := surface.createAlertmanager(t, "Prometheus Alertmanager")
	recorder := startIntakeRecorder(t, intakeAddress)
	configuration := documentedConfiguration(t, deployment{
		integration: created.Integration.ID,
		secret:      created.WebhookSecret,
		origin:      recorder.origin(t),
	})

	return &alertmanagerGate{
		integrationPlane: surface,
		integration:      created.Integration.ID,
		secret:           created.WebhookSecret,
		recorder:         recorder,
		alertmanager:     startAlertmanagerContainer(t, configuration, recorder.port(t)),
		prompts:          prompts,
	}
}

type forwarded struct {
	Body    []byte
	Token   string
	Status  int
	Payload deliveredPayload
}

type deliveredPayload struct {
	Status   string `json:"status"`
	GroupKey string `json:"groupKey"`
	Alerts   []struct {
		Status       string            `json:"status"`
		Fingerprint  string            `json:"fingerprint"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generatorURL"`
		StartsAt     time.Time         `json:"startsAt"`
		EndsAt       time.Time         `json:"endsAt"`
	} `json:"alerts"`
}

type intakeRecorder struct {
	server *httptest.Server

	mu               sync.Mutex
	deliveries       []forwarded
	backpressureNext bool
	failNext         bool
}

func startIntakeRecorder(t *testing.T, intakeAddress string) *intakeRecorder {
	t.Helper()

	recorder := &intakeRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "unreadable body", http.StatusBadRequest)
				return
			}
			if recorder.takeBackpressure() {
				recorder.record(forwarded{
					Body: body, Token: request.Header.Get(intake.TokenHeader),
					Status: http.StatusServiceUnavailable,
				})
				writer.Header().Set("Retry-After", "1")
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			forward, err := http.NewRequestWithContext(request.Context(), request.Method,
				"http://"+intakeAddress+request.URL.RequestURI(), bytes.NewReader(body))
			if err != nil {
				http.Error(writer, "unforwardable", http.StatusInternalServerError)
				return
			}
			forward.Header = request.Header.Clone()

			answer, err := http.DefaultClient.Do(forward)
			if err != nil {
				http.Error(writer, "intake unreachable", http.StatusBadGateway)
				return
			}
			defer func() { _ = answer.Body.Close() }()

			recorder.record(forwarded{
				Body:   body,
				Token:  request.Header.Get(intake.TokenHeader),
				Status: answer.StatusCode,
			})

			if recorder.takeFailure() {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(answer.StatusCode)
			_, _ = io.Copy(writer, answer.Body)
		}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (r *intakeRecorder) backpressureNextDelivery() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backpressureNext = true
}

func (r *intakeRecorder) takeBackpressure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	backpressure := r.backpressureNext
	r.backpressureNext = false
	return backpressure
}

func (r *intakeRecorder) record(delivery forwarded) {
	_ = json.Unmarshal(delivery.Body, &delivery.Payload)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, delivery)
}

func (r *intakeRecorder) failNextDelivery() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext = true
}

func (r *intakeRecorder) takeFailure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	failing := r.failNext
	r.failNext = false
	return failing
}

func (r *intakeRecorder) matching(alertname, status string) []forwarded {
	r.mu.Lock()
	defer r.mu.Unlock()

	var found []forwarded
	for _, delivery := range r.deliveries {
		for _, alert := range delivery.Payload.Alerts {
			if alert.Labels["alertname"] == alertname && alert.Status == status {
				found = append(found, delivery)
				break
			}
		}
	}
	return found
}

const (
	awaitQuiet = 90 * time.Second
	awaitTotal = 8 * time.Minute
)

func (r *intakeRecorder) await(t *testing.T, alertname, status string, count int) []forwarded {
	t.Helper()

	started := time.Now()
	lastArrival := time.Now()
	seen := r.count()
	for {
		if found := r.matching(alertname, status); len(found) >= count {
			return found
		}
		if now := r.count(); now != seen {
			seen, lastArrival = now, time.Now()
		}
		if verdict := awaitVerdict(seen, count, alertname, status,
			time.Since(lastArrival), time.Since(started)); verdict != "" {
			t.Fatal(verdict)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func awaitVerdict(
	seen, want int, alertname, status string, quiet, elapsed time.Duration,
) string {
	switch {
	case quiet > awaitQuiet && seen == 0:
		return fmt.Sprintf("alertmanager delivered nothing at all in %s; the documented "+
			"configuration did not reach intake", elapsed.Round(time.Second))
	case quiet > awaitQuiet:
		return fmt.Sprintf("alertmanager delivered %d times but %s %s fewer than %d, and "+
			"nothing new arrived for %s; the configuration reaches intake and this state "+
			"did not follow", seen, status, alertname, want, awaitQuiet)
	case elapsed > awaitTotal:
		return fmt.Sprintf("gave up after %s waiting for %s %s (%d deliveries arrived); "+
			"this is a timeout on a loaded runner, not a configuration result",
			awaitTotal, status, alertname, seen)
	default:
		return ""
	}
}

func (r *intakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deliveries)
}

func (r *intakeRecorder) port(t *testing.T) int {
	t.Helper()

	parsed, err := url.Parse(r.server.URL)
	if err != nil {
		t.Fatalf("reading the recorder address: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("reading the recorder port: %v", err)
	}
	return port
}

func (r *intakeRecorder) origin(t *testing.T) string {
	t.Helper()
	return "http://" + net.JoinHostPort(testcontainers.HostInternal, strconv.Itoa(r.port(t)))
}

func startAlertmanagerContainer(t *testing.T, configuration string, hostPort int) string {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "alertmanager.yml")
	if err := os.WriteFile(path, []byte(configuration), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        alertmanagerGateImage,
			ExposedPorts: []string{"9093/tcp"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      path,
				ContainerFilePath: "/etc/alertmanager/alertmanager.yml",
				FileMode:          0o644,
			}},
			HostAccessPorts: []int{hostPort},
			WaitingFor: wait.ForHTTP("/-/ready").WithPort("9093/tcp").
				WithStartupTimeout(3 * time.Minute),
		},
	})
	if err != nil {
		t.Fatalf("starting %s on the documented configuration: %v\n%s",
			alertmanagerGateImage, err, configuration)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminating alertmanager: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("reading the alertmanager host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9093/tcp")
	if err != nil {
		t.Fatalf("reading the alertmanager port: %v", err)
	}
	return "http://" + net.JoinHostPort(host, port.Port())
}

func (g *alertmanagerGate) fire(t *testing.T, alerts ...map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatalf("encoding the alerts: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.alertmanager+"/api/v2/alerts", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("building the alert: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("firing the alert: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("alertmanager refused the alert = %d: %s", response.StatusCode, answer)
	}
}

func firingAlert(alertname string, began time.Time) map[string]any {
	return map[string]any{
		"labels": map[string]string{
			"alertname": alertname, "severity": "critical", "namespace": "payments",
		},
		"annotations": map[string]string{
			"summary":       "the payments node stopped reporting",
			"runbook_url":   "https://runbooks.acme.example/" + alertname,
			"dashboard_url": "https://grafana.acme.example/d/" + alertname,
		},
		"startsAt":     began.Format(time.RFC3339),
		"endsAt":       began.Add(time.Hour).Format(time.RFC3339),
		"generatorURL": "https://prometheus.acme.example/graph?g0.expr=up",
	}
}

func resolvedAlert(alertname string, began, ended time.Time) map[string]any {
	alert := firingAlert(alertname, began)
	alert["endsAt"] = ended.Format(time.RFC3339)
	return alert
}

func (g *alertmanagerGate) replay(t *testing.T, secret string, body []byte) int {
	t.Helper()
	status, _ := g.deliver(t, g.integration, secret, body)
	return status
}

func (g *alertmanagerGate) incident(t *testing.T, id string) incidentBody {
	t.Helper()

	status, body := g.call(t, http.MethodGet, g.base(surfaceOrg)+"/incidents/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("reading incident %s = %d: %s", id, status, body)
	}
	var incident incidentBody
	decodeInto(t, body, &incident)
	return incident
}

func (g *alertmanagerGate) incidents(t *testing.T) incidentListBody {
	t.Helper()

	status, body := g.call(t, http.MethodGet, g.base(surfaceOrg)+"/incidents", nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents = %d: %s", status, body)
	}
	var list incidentListBody
	decodeInto(t, body, &list)
	return list
}

func (g *alertmanagerGate) alertEventsNamed(t *testing.T, alertname string) []recordedAlertEvent {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, g.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	rows, err := connection.Query(ctx, `
		SELECT source_key, status, title, summary, labels, annotations, generator_url,
		       started_at, resolved_at, received_at
		  FROM alert_event
		 WHERE org_id = $1 AND labels ->> 'alertname' = $2
		 ORDER BY received_at`, surfaceOrg, alertname)
	if err != nil {
		t.Fatalf("reading alertEvents: %v", err)
	}
	defer rows.Close()

	var recorded []recordedAlertEvent
	for rows.Next() {
		var alertEvent recordedAlertEvent
		var labels, annotations []byte
		if err = rows.Scan(&alertEvent.SourceKey, &alertEvent.Status, &alertEvent.Title, &alertEvent.Summary,
			&labels, &annotations, &alertEvent.GeneratorURL, &alertEvent.StartedAt,
			&alertEvent.ResolvedAt, &alertEvent.ReceivedAt); err != nil {
			t.Fatalf("scanning a alert_event: %v", err)
		}
		if err = json.Unmarshal(labels, &alertEvent.Labels); err != nil {
			t.Fatalf("decoding labels: %v", err)
		}
		if err = json.Unmarshal(annotations, &alertEvent.Annotations); err != nil {
			t.Fatalf("decoding annotations: %v", err)
		}
		recorded = append(recorded, alertEvent)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("reading alertEvents: %v", err)
	}
	return recorded
}

func (g *alertmanagerGate) countDeliveries(t *testing.T) int {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, g.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	var counted int
	err = connection.QueryRow(ctx, `
		SELECT count(*) FROM webhook_delivery
		 WHERE integration_id = $1`, g.integration).Scan(&counted)
	if err != nil {
		t.Fatalf("counting deliveries: %v", err)
	}
	return counted
}

func (g *alertmanagerGate) setEnabled(t *testing.T, enabled bool) {
	t.Helper()

	operation := "disable"
	if enabled {
		operation = "enable"
	}
	status, body := g.call(t, http.MethodPost,
		g.base(surfaceOrg)+"/integrations/"+g.integration+"/"+operation, nil)
	if status != http.StatusNoContent {
		t.Fatalf("setting enabled=%v = %d: %s", enabled, status, body)
	}
}

func TestTheWaitSaysWhichKindOfFailureItIs(t *testing.T) {
	t.Parallel()

	verdict := awaitVerdict(0, 1, "PoolExhausted", "resolved",
		awaitQuiet+time.Second, awaitQuiet+time.Second)
	if !strings.Contains(verdict, "did not reach intake") {
		t.Errorf("a silent pipe must say the configuration did not reach intake: %q", verdict)
	}

	verdict = awaitVerdict(3, 1, "PoolExhausted", "resolved",
		awaitQuiet+time.Second, awaitQuiet+time.Second)
	if strings.Contains(verdict, "did not reach intake") {
		t.Errorf("deliveries DID reach intake; the verdict must not say otherwise: %q", verdict)
	}
	if !strings.Contains(verdict, "reaches intake") {
		t.Errorf("the verdict does not say what did work: %q", verdict)
	}

	verdict = awaitVerdict(9, 1, "PoolExhausted", "resolved",
		time.Second, awaitTotal+time.Second)
	if !strings.Contains(verdict, "not a configuration result") {
		t.Errorf("a runner that never stopped delivering has proved nothing about the "+
			"configuration, and the verdict must say so: %q", verdict)
	}

	if verdict = awaitVerdict(2, 1, "PoolExhausted", "resolved",
		30*time.Second, 5*time.Minute); verdict != "" {
		t.Errorf("a slow but moving pipe was failed: %q", verdict)
	}
}

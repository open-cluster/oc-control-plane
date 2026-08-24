package main

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
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The harness behind the Alertmanager compatibility gate: the documentation page read as
// configuration, a real Alertmanager running on it, a recorder in the delivery path, and
// the readers the gate asserts through. The sentences it exists for are next door in
// alertmanager_gate_test.go.

const (
	// alertmanagerGateImage is THE supported version. It is stated in the documentation as
	// the version this gate proves, and raising it is a deliberate edit with the gate re-run
	// — not something that drifts because a tag moved.
	alertmanagerGateImage = "prom/alertmanager:v0.34.0"
)

// alertmanagerGate is the composed product with a real Alertmanager in front of it: the
// operator surface, intake, a real database, a scripted model boundary so this gate never
// pays a provider, and a recorder in the delivery path.
type alertmanagerGate struct {
	*integrationPlane
	integration  string
	secret       string
	recorder     *intakeRecorder
	alertmanager string
	investigator *scriptedInvestigatorMain
}

func startAlertmanagerGate(t *testing.T) *alertmanagerGate {
	t.Helper()

	investigator := &scriptedInvestigatorMain{exchange: &scriptedExchangeMain{}}
	operatorAddress := freeAddress(t)
	intakeAddress := freeAddress(t)
	var dsn string
	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = surfaceOrg
		dsn = cfg.Placements["shared"]
	}, app.Options{Investigator: investigator})

	surface := &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress, dsn: dsn,
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
		investigator:     investigator,
	}
}

// forwarded is one delivery as it passed through the recorder: the body and token
// Alertmanager sent, and the answer intake gave.
type forwarded struct {
	Body    []byte
	Token   string
	Status  int
	Payload deliveredPayload
}

// deliveredPayload is the v4 webhook body as Alertmanager renders it. Only what this gate
// asserts on is declared.
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

// intakeRecorder stands between Alertmanager and intake, forwarding every request verbatim
// and returning intake's own answer.
//
// It exists for two reasons, both needing Alertmanager's own body rather than one this test
// wrote. A delivery is stored as a digest and never as a body, so the only place to capture
// what Alertmanager actually sent is in flight. And it can be told to answer one delivery
// with a server error AFTER intake has already accepted it, which is the only honest way to
// provoke Alertmanager's own retry of an identical body.
type intakeRecorder struct {
	server *httptest.Server

	mu         sync.Mutex
	deliveries []forwarded
	failNext   bool
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

func (r *intakeRecorder) record(delivery forwarded) {
	// A body that does not decode is still recorded: what it was is exactly what a failing
	// assertion needs to report.
	_ = json.Unmarshal(delivery.Body, &delivery.Payload)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, delivery)
}

// failNextDelivery tells the recorder to answer the next delivery with a server error after
// intake has already accepted it.
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

// matching reports the deliveries carrying one alert in one state.
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

// The gate's waiting budgets. quiet is how long NOTHING may arrive before the wait is
// given up; total is the backstop so a wait cannot run until the suite's own timeout.
//
// The budget is IDLE time rather than total time, and that distinction is the whole point.
// A total budget cannot tell a configuration that does not work from a runner that is
// merely busy: this gate takes about 37s alone and has taken over 114s inside a full suite
// competing for one container runtime, and what it printed when it lost that race was "the
// documented configuration did not reach intake" — a sentence about a configuration that
// was fine. That is the mirror of the defect #16 closed: there, CI could report success
// over a suite that never ran; here, it could report failure over a product that works.
//
// Resetting on ANY new delivery keeps the honest failure fast. A pipe that is moving keeps
// its time; a pipe that is silent still fails in 90 seconds, which is the case that means
// something.
const (
	awaitQuiet = 90 * time.Second
	awaitTotal = 8 * time.Minute
)

// await blocks until at least count deliveries of one alert in one state have been
// forwarded, and returns them.
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
			// Something arrived. The pipe is working and this runner is just slow.
			seen, lastArrival = now, time.Now()
		}
		if verdict := awaitVerdict(seen, count, alertname, status,
			time.Since(lastArrival), time.Since(started)); verdict != "" {
			t.Fatal(verdict)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// awaitVerdict decides whether a wait has ended and says why, or returns empty while it
// should carry on. It is a pure function so that all three endings can be tested without
// waiting eight minutes for one of them — which matters here, because the WORDS are the
// thing being fixed: the old budget's failure said "the documented configuration did not
// reach intake" about configurations that were fine.
func awaitVerdict(
	seen, want int, alertname, status string, quiet, elapsed time.Duration,
) string {
	switch {
	case quiet > awaitQuiet && seen == 0:
		// Nothing has arrived at all. This is the failure that means something, and it
		// is still prompt: a broken configuration does not get eight minutes.
		return fmt.Sprintf("alertmanager delivered nothing at all in %s; the documented "+
			"configuration did not reach intake", elapsed.Round(time.Second))
	case quiet > awaitQuiet:
		return fmt.Sprintf("alertmanager delivered %d times but %s %s fewer than %d, and "+
			"nothing new arrived for %s; the configuration reaches intake and this state "+
			"did not follow", seen, status, alertname, want, awaitQuiet)
	case elapsed > awaitTotal:
		// Deliveries kept arriving and the awaited one never did. Said as a give-up
		// rather than as a verdict on the configuration, because a runner this slow has
		// not proved anything about it either way.
		return fmt.Sprintf("gave up after %s waiting for %s %s (%d deliveries arrived); "+
			"this is a timeout on a loaded runner, not a configuration result",
			awaitTotal, status, alertname, seen)
	default:
		return ""
	}
}

// count reports how many deliveries have arrived in all.
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

// origin is the recorder as the container reaches it, which is what a customer substitutes
// for the example origin on the page.
func (r *intakeRecorder) origin(t *testing.T) string {
	t.Helper()
	return "http://" + net.JoinHostPort(testcontainers.HostInternal, strconv.Itoa(r.port(t)))
}

// startAlertmanagerContainer runs the pinned Alertmanager on the documented configuration and
// returns its API base URL. The host port is exposed to the container, so the endpoint in the
// configuration is one the container can genuinely reach.
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
		// Not a skip. By the time this runs the suite has already started Postgres in a
		// container, so a container runtime exists and this is a real failure — most likely
		// a configuration the documented page can no longer produce.
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

// fire posts alerts through Alertmanager's own API, which is how a customer's Prometheus
// reaches it — so what arrives at intake is Alertmanager's body, not this test's.
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

// firingAlert is one alert as a customer's Prometheus posts it to their own Alertmanager,
// carrying everything an investigation wants to start from.
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
		"startsAt": began.Format(time.RFC3339),
		// A firing alert states when it stops being true, as a Prometheus that keeps
		// re-posting it does. Leaving it out would hand Alertmanager its own resolve
		// timeout instead, and a slow run would then deliver a resolution nobody asked
		// for — a clock this gate does not control deciding what it observes.
		"endsAt":       began.Add(time.Hour).Format(time.RFC3339),
		"generatorURL": "https://prometheus.acme.example/graph?g0.expr=up",
	}
}

// resolvedAlert is the same alert with an end time, which is how a source tells its own
// Alertmanager the failure stopped. The start time is unchanged on purpose: it identifies
// the episode being closed.
func resolvedAlert(alertname string, began, ended time.Time) map[string]any {
	alert := firingAlert(alertname, began)
	alert["endsAt"] = ended.Format(time.RFC3339)
	return alert
}

// replay posts a body straight to intake, bypassing Alertmanager. Used only for bodies
// Alertmanager already produced, and for the one mangled body it will not produce on request.
func (g *alertmanagerGate) replay(t *testing.T, secret string, body []byte) int {
	t.Helper()
	status, _ := g.deliver(t, g.integration, secret, body)
	return status
}

// episode reads one incident through the operator API an operator would read it through.
func (g *alertmanagerGate) episode(t *testing.T, id string) episodeBody {
	t.Helper()

	status, body := g.call(t, http.MethodGet, g.base(surfaceOrg)+"/incidents/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("reading incident %s = %d: %s", id, status, body)
	}
	var episode episodeBody
	decodeInto(t, body, &episode)
	return episode
}

// episodes lists this organization's incidents.
func (g *alertmanagerGate) episodes(t *testing.T) episodeListBody {
	t.Helper()

	status, body := g.call(t, http.MethodGet, g.base(surfaceOrg)+"/incidents", nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents = %d: %s", status, body)
	}
	var list episodeListBody
	decodeInto(t, body, &list)
	return list
}

// signalsNamed reports what is durably recorded for one alert name.
func (g *alertmanagerGate) signalsNamed(t *testing.T, alertname string) []recordedSignal {
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
		  FROM signal
		 WHERE org_id = $1 AND labels ->> 'alertname' = $2
		 ORDER BY received_at`, surfaceOrg, alertname)
	if err != nil {
		t.Fatalf("reading signals: %v", err)
	}
	defer rows.Close()

	var recorded []recordedSignal
	for rows.Next() {
		var signal recordedSignal
		var labels, annotations []byte
		if err = rows.Scan(&signal.SourceKey, &signal.Status, &signal.Title, &signal.Summary,
			&labels, &annotations, &signal.GeneratorURL, &signal.StartedAt,
			&signal.ResolvedAt, &signal.ReceivedAt); err != nil {
			t.Fatalf("scanning a signal: %v", err)
		}
		if err = json.Unmarshal(labels, &signal.Labels); err != nil {
			t.Fatalf("decoding labels: %v", err)
		}
		if err = json.Unmarshal(annotations, &signal.Annotations); err != nil {
			t.Fatalf("decoding annotations: %v", err)
		}
		recorded = append(recorded, signal)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("reading signals: %v", err)
	}
	return recorded
}

// countDeliveries reports how many delivery attempts this Integration recorded with one
// disposition and reason. This record is what makes "no alerts arrived" distinguishable from
// "alerts were turned away", so the gate counts each kind rather than counting attempts.
//
// The disposition and the refusal reasons are storage's own, never a second copy: the
// persisted values are frozen there, and a copy here would drift without anything noticing.
func (g *alertmanagerGate) countDeliveries(
	t *testing.T, disposition storage.DeliveryDisposition, reason string,
) int {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, g.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	var counted int
	err = connection.QueryRow(ctx, `
		SELECT count(*) FROM integration_delivery
		 WHERE integration_id = $1 AND outcome = $2 AND reason = $3`,
		g.integration, int16(disposition), reason).Scan(&counted)
	if err != nil {
		t.Fatalf("counting deliveries: %v", err)
	}
	return counted
}

// setEnabled turns this gate's Integration on or off through the surface an operator uses.
func (g *alertmanagerGate) setEnabled(t *testing.T, enabled bool) {
	t.Helper()

	status, body := g.call(t, http.MethodPost,
		g.base(surfaceOrg)+"/integrations/"+g.integration+"/enabled",
		map[string]any{"enabled": enabled})
	if status != http.StatusNoContent {
		t.Fatalf("setting enabled=%v = %d: %s", enabled, status, body)
	}
}

// The three endings, and which sentence each one says.
//
// The words are the fix. A gate that fails a working configuration is bad; a gate that
// fails a working configuration while SAYING the configuration is broken sends whoever
// reads it to the wrong place entirely, and on a release day that is expensive.
func TestTheWaitSaysWhichKindOfFailureItIs(t *testing.T) {
	t.Parallel()

	// Nothing arrived: the honest product failure, and still prompt.
	verdict := awaitVerdict(0, 1, "PoolExhausted", "resolved",
		awaitQuiet+time.Second, awaitQuiet+time.Second)
	if !strings.Contains(verdict, "did not reach intake") {
		t.Errorf("a silent pipe must say the configuration did not reach intake: %q", verdict)
	}

	// Deliveries arrived, this state did not, and the pipe then went quiet. Still a
	// failure, but not one that blames the configuration for being unreachable.
	verdict = awaitVerdict(3, 1, "PoolExhausted", "resolved",
		awaitQuiet+time.Second, awaitQuiet+time.Second)
	if strings.Contains(verdict, "did not reach intake") {
		t.Errorf("deliveries DID reach intake; the verdict must not say otherwise: %q", verdict)
	}
	if !strings.Contains(verdict, "reaches intake") {
		t.Errorf("the verdict does not say what did work: %q", verdict)
	}

	// Still arriving after the backstop: a loaded runner, said as one.
	verdict = awaitVerdict(9, 1, "PoolExhausted", "resolved",
		time.Second, awaitTotal+time.Second)
	if !strings.Contains(verdict, "not a configuration result") {
		t.Errorf("a runner that never stopped delivering has proved nothing about the "+
			"configuration, and the verdict must say so: %q", verdict)
	}

	// The case the flake produced: slow, but still moving, and well inside the backstop.
	// This is the one that used to fail, and it must now say nothing at all.
	if verdict = awaitVerdict(2, 1, "PoolExhausted", "resolved",
		30*time.Second, 5*time.Minute); verdict != "" {
		t.Errorf("a slow but moving pipe was failed: %q", verdict)
	}
}

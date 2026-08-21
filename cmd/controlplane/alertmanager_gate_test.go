package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"gopkg.in/yaml.v3"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
)

// The compatibility gate for the front door.
//
// Every other test of intake posts a body this repository wrote in the shape it believes
// Alertmanager sends. That proves the parser handles what we believe, which is exactly the
// belief worth checking. Here a REAL Alertmanager is started, handed the configuration the
// documentation page gives customers, and told to fire an alert; the body under test is
// Alertmanager's own.
//
// The configuration is not written twice. It is read out of the page, and only the three
// values a customer substitutes are substituted. If somebody edits the page into something
// that does not work, or changes intake so the documented configuration no longer reaches
// it, this fails — which is the point, because the failure it guards against is a customer
// whose first alert vanished, and the symptom of that is silence.

const (
	// alertmanagerGateImage is THE supported version. It is stated in the documentation as
	// the version this gate proves, and raising it is a deliberate edit with the gate re-run
	// — not something that drifts because a tag moved.
	alertmanagerGateImage = "prom/alertmanager:v0.34.0"

	// alertmanagerDocPage is the page a customer copies from. Its YAML is this test's input.
	alertmanagerDocPage = "../../docs/integrations/alerting/alertmanager.mdx"

	// alertmanagerDocOrigin is the example intake origin the page publishes, and one of the
	// three deployment-specific values a customer replaces.
	alertmanagerDocOrigin = "https://oc.example.com"

	// idleReceiver stands in for the default receiver a customer's existing configuration
	// already has. The documented fragment deliberately carries no root receiver — it says
	// "add the receiver, then route alerts to it" — and Alertmanager refuses a configuration
	// without one.
	idleReceiver = "the-customers-existing-default"
)

// alertmanagerGate is the composed product with a real Alertmanager in front of it: the
// operator surface, intake, a real database, a scripted model boundary so this gate never
// pays a provider, and a recorder in the delivery path.
type alertmanagerGate struct {
	*integrationPlane
	dsn          string
	integration  string
	secret       string
	recorder     *intakeRecorder
	alertmanager string
	investigator *scriptedInvestigatorMain
}

func startAlertmanagerGate(t *testing.T) *alertmanagerGate {
	t.Helper()

	investigator := &scriptedInvestigatorMain{conversation: &scriptedConversationMain{}}
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
	}, wiring{investigator: investigator})

	surface := &integrationPlane{
		controlPlane: plane, operator: operatorAddress, intake: intakeAddress,
	}
	created := surface.createAlertmanager(t, "Prometheus Alertmanager")
	recorder := startIntakeRecorder(t, intakeAddress)
	configuration := documentedConfiguration(t,
		created.Integration.ID, created.WebhookSecret, recorder.origin(t))

	return &alertmanagerGate{
		integrationPlane: surface,
		dsn:              dsn,
		integration:      created.Integration.ID,
		secret:           created.WebhookSecret,
		recorder:         recorder,
		alertmanager:     startAlertmanagerContainer(t, configuration, recorder.port(t)),
		investigator:     investigator,
	}
}

// documentedReceiver returns the one fenced YAML block on the documentation page that
// configures a webhook receiver, dedented to column zero.
//
// Extraction is asserted rather than attempted. A page that no longer carries exactly one
// such block fails the build, because the alternative — quietly falling back to a copy of
// the YAML held here — is the failure this whole gate exists to prevent.
func documentedReceiver(t *testing.T) string {
	t.Helper()

	page, err := os.ReadFile(filepath.FromSlash(alertmanagerDocPage))
	if err != nil {
		t.Fatalf("reading the documentation page: %v", err)
	}

	var receivers []string
	lines := strings.Split(strings.ReplaceAll(string(page), "\r\n", "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "```yaml" {
			continue
		}
		indent := lines[index][:strings.Index(lines[index], "`")]
		var block []string
		for index++; index < len(lines) && strings.TrimSpace(lines[index]) != "```"; index++ {
			block = append(block, strings.TrimPrefix(lines[index], indent))
		}
		if joined := strings.Join(block, "\n"); strings.Contains(joined, "webhook_configs") {
			receivers = append(receivers, joined)
		}
	}

	if len(receivers) != 1 {
		t.Fatalf("%s carries %d webhook receiver configurations, want exactly 1: this gate "+
			"runs the configuration the page publishes and must never fall back to a copy",
			alertmanagerDocPage, len(receivers))
	}
	return receivers[0]
}

// documentedConfiguration renders the configuration a customer following the page would
// actually be running: the documented lines with the three deployment-specific values
// substituted, plus the root route their existing configuration already has.
func documentedConfiguration(t *testing.T, integrationID, secret, origin string) string {
	t.Helper()

	documented := documentedReceiver(t)
	for _, placeholder := range []string{
		"<integration-id>", "<webhook-secret>", alertmanagerDocOrigin,
	} {
		if !strings.Contains(documented, placeholder) {
			t.Fatalf("the documented configuration no longer carries %q, so this gate can no "+
				"longer substitute what a customer substitutes:\n%s", placeholder, documented)
		}
	}
	// Three substitutions, and only three. The scheme becomes plain HTTP with the origin
	// because terminating TLS is not what this gate proves; everything else is the page's
	// own text.
	substituted := strings.NewReplacer(
		"<integration-id>", integrationID,
		"<webhook-secret>", secret,
		alertmanagerDocOrigin, origin,
	).Replace(documented)

	assertDocumentedReceiver(t, substituted, origin, integrationID, secret)
	return withCustomerRootRoute(t, substituted)
}

// assertDocumentedReceiver checks that what was extracted is a usable receiver rather than
// prose that happened to mention a webhook. Each failure here names a way the page could rot
// while still looking like a configuration.
func assertDocumentedReceiver(t *testing.T, configuration, origin, integrationID, secret string) {
	t.Helper()

	var documented struct {
		Receivers []struct {
			Name           string `yaml:"name"`
			WebhookConfigs []struct {
				URL          string `yaml:"url"`
				SendResolved *bool  `yaml:"send_resolved"`
				HTTPConfig   struct {
					HTTPHeaders map[string]struct {
						Secrets []string `yaml:"secrets"`
					} `yaml:"http_headers"`
				} `yaml:"http_config"`
			} `yaml:"webhook_configs"`
		} `yaml:"receivers"`
		Route struct {
			Routes []struct {
				Receiver string `yaml:"receiver"`
			} `yaml:"routes"`
		} `yaml:"route"`
	}
	if err := yaml.Unmarshal([]byte(configuration), &documented); err != nil {
		t.Fatalf("the documented configuration is not YAML: %v\n%s", err, configuration)
	}

	var name string
	var webhooks int
	for _, receiver := range documented.Receivers {
		for _, webhook := range receiver.WebhookConfigs {
			webhooks++
			name = receiver.Name
			want := origin + "/intake/v1/integrations/" + integrationID + "/signals"
			if webhook.URL != want {
				t.Errorf("the documented webhook url is %q, want %q; the page must point a "+
					"customer at the endpoint intake actually serves", webhook.URL, want)
			}
			if webhook.SendResolved == nil || !*webhook.SendResolved {
				t.Error("the documented receiver does not set send_resolved: true, so a " +
					"customer's resolved alerts would never arrive and their incident list " +
					"would never close")
			}
			header, ok := webhook.HTTPConfig.HTTPHeaders[intake.TokenHeader]
			if !ok {
				t.Fatalf("the documented receiver sets no %s header, so every delivery a "+
					"customer makes would be refused", intake.TokenHeader)
			}
			if len(header.Secrets) != 1 || header.Secrets[0] != secret {
				t.Errorf("the documented %s header carries %v, want the webhook secret",
					intake.TokenHeader, header.Secrets)
			}
		}
	}
	if webhooks != 1 {
		t.Fatalf("the documented configuration carries %d webhook configs, want exactly 1",
			webhooks)
	}

	routed := false
	for _, route := range documented.Route.Routes {
		routed = routed || route.Receiver == name
	}
	if !routed {
		t.Fatalf("the documented route does not send anything to the %q receiver, so a "+
			"customer who pasted this page would receive nothing", name)
	}
}

// withCustomerRootRoute adds what the customer's own configuration supplies around the
// documented fragment, and nothing else. The documented lines are never edited: what is
// added is a root receiver, which Alertmanager requires and the fragment deliberately omits,
// grouping by alert name, and group timings short enough that this gate does not spend
// Alertmanager's default thirty-second group wait on every alert.
func withCustomerRootRoute(t *testing.T, documented string) string {
	t.Helper()

	var whole map[string]any
	if err := yaml.Unmarshal([]byte(documented), &whole); err != nil {
		t.Fatalf("the documented configuration is not YAML: %v\n%s", err, documented)
	}

	route, ok := whole["route"].(map[string]any)
	if !ok {
		t.Fatalf("the documented configuration carries no route to attach to:\n%s", documented)
	}
	route["receiver"] = idleReceiver
	route["group_by"] = []string{"alertname"}
	route["group_wait"] = "1s"
	route["group_interval"] = "1s"

	receivers, ok := whole["receivers"].([]any)
	if !ok {
		t.Fatalf("the documented configuration carries no receivers:\n%s", documented)
	}
	whole["receivers"] = append(receivers, map[string]any{"name": idleReceiver})

	rendered, err := yaml.Marshal(whole)
	if err != nil {
		t.Fatalf("rendering the configuration: %v", err)
	}
	return string(rendered)
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

// await blocks until at least count deliveries of one alert in one state have been
// forwarded, and returns them.
func (r *intakeRecorder) await(t *testing.T, alertname, status string, count int) []forwarded {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for {
		if found := r.matching(alertname, status); len(found) >= count {
			return found
		}
		if time.Now().After(deadline) {
			r.mu.Lock()
			seen := len(r.deliveries)
			r.mu.Unlock()
			t.Fatalf("alertmanager delivered %s %s fewer than %d times (%d deliveries in "+
				"all); the documented configuration did not reach intake",
				status, alertname, count, seen)
		}
		time.Sleep(200 * time.Millisecond)
	}
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
		"startsAt":     began.Format(time.RFC3339),
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

// The recorded delivery outcomes, as the schema numbers them.
const (
	deliveryAccepted  int16 = 1
	deliveryDuplicate int16 = 2
	deliveryRejected  int16 = 3
)

// deliveryOutcome is one recorded delivery attempt: what was decided and, for a refusal, why.
type deliveryOutcome struct {
	Outcome int16
	Reason  string
}

// outcomes reports every recorded delivery attempt for this integration, oldest first. This
// is the record that makes "no alerts arrived" distinguishable from "alerts were turned away".
func (g *alertmanagerGate) outcomes(t *testing.T) []deliveryOutcome {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, g.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	rows, err := connection.Query(ctx, `
		SELECT outcome, reason FROM integration_delivery
		 WHERE integration_id = $1 ORDER BY received_at, delivery_id`, g.integration)
	if err != nil {
		t.Fatalf("reading deliveries: %v", err)
	}
	defer rows.Close()

	var recorded []deliveryOutcome
	for rows.Next() {
		var outcome deliveryOutcome
		if err = rows.Scan(&outcome.Outcome, &outcome.Reason); err != nil {
			t.Fatalf("scanning a delivery: %v", err)
		}
		recorded = append(recorded, outcome)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("reading deliveries: %v", err)
	}
	return recorded
}

// countOutcome reports how many delivery attempts were recorded with one outcome and reason.
func (g *alertmanagerGate) countOutcome(t *testing.T, want int16, reason string) int {
	t.Helper()

	counted := 0
	for _, outcome := range g.outcomes(t) {
		if outcome.Outcome == want && outcome.Reason == reason {
			counted++
		}
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

// THE GATE. A real alert, fired through a real Alertmanager configured exactly as the
// documentation page says, becomes an incident an SRE can investigate — and closes when the
// alert does.
func TestAlertmanagerGate_TheDocumentedConfigurationDeliversAnInvestigableIncident(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const alertname = "GateNodeNotReady"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	gate.fire(t, firingAlert(alertname, began))
	delivered := gate.recorder.await(t, alertname, "firing", 1)[0]

	if delivered.Status != http.StatusAccepted {
		t.Fatalf("intake answered alertmanager %d, want 202; the documented configuration "+
			"did not authenticate:\n%s", delivered.Status, delivered.Body)
	}
	if delivered.Token != gate.secret {
		t.Fatalf("alertmanager presented %q in %s, want the webhook secret; the documented "+
			"header configuration is wrong for %s",
			delivered.Token, intake.TokenHeader, alertmanagerGateImage)
	}

	// The Signal is what the alert became. Everything the customer's alerting already knew
	// has to survive intake, because it is what the investigation starts from.
	signals := gate.signalsNamed(t, alertname)
	if len(signals) != 1 {
		t.Fatalf("one fired alert produced %d signals, want 1", len(signals))
	}
	signal := signals[0]
	if signal.SourceKey != delivered.Payload.Alerts[0].Fingerprint {
		t.Errorf("the signal is keyed %q, want alertmanager's own fingerprint %q",
			signal.SourceKey, delivered.Payload.Alerts[0].Fingerprint)
	}
	if signal.Title != alertname {
		t.Errorf("the signal is titled %q, want %q", signal.Title, alertname)
	}
	if signal.Labels["severity"] != "critical" || signal.Labels["namespace"] != "payments" {
		t.Errorf("the alert's labels did not survive intake: %v", signal.Labels)
	}
	if signal.Annotations["runbook_url"] == "" || signal.Annotations["dashboard_url"] == "" {
		t.Errorf("the runbook and dashboard the operator wrote did not survive intake: %v",
			signal.Annotations)
	}
	if signal.GeneratorURL == "" {
		t.Error("the alert's generator url did not survive intake")
	}
	if !signal.StartedAt.Equal(began) {
		t.Errorf("the signal started at %s, want the alert's own %s", signal.StartedAt, began)
	}

	// The incident, grouped on the identity ALERTMANAGER supplied. Nothing here infers it.
	episodeID := gate.episodeByTitle(t, alertname)
	episode := gate.episode(t, episodeID)
	if episode.Grouping.Basis != "source_grouping" {
		t.Errorf("the episode's grouping basis is %q, want source_grouping",
			episode.Grouping.Basis)
	}
	if episode.Grouping.Key != delivered.Payload.GroupKey {
		t.Errorf("the episode is grouped on %q, want alertmanager's own group key %q",
			episode.Grouping.Key, delivered.Payload.GroupKey)
	}
	if episode.Status != "open" {
		t.Errorf("the episode is %q while its alert fires, want open", episode.Status)
	}

	// An investigation opens against it, and starts from what the alert carried.
	status, body := gate.call(t, http.MethodPost, gate.base(surfaceOrg)+"/investigations",
		map[string]any{"episodeId": episodeID})
	if status != http.StatusAccepted {
		t.Fatalf("opening an investigation on the episode = %d: %s", status, body)
	}
	var opened struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &opened)
	gate.awaitInvestigation(t, opened.ID)

	trigger := gate.investigator.orientation.Trigger
	if trigger == nil {
		t.Fatal("the investigation started with no trigger; an incident from an alert must " +
			"begin with what the alert said")
	}
	if trigger.Title != alertname {
		t.Errorf("the trigger is titled %q, want %q", trigger.Title, alertname)
	}
	if trigger.Labels["namespace"] != "payments" {
		t.Errorf("the trigger carries labels %v, want the alert's own", trigger.Labels)
	}
	if trigger.Annotations["runbook_url"] == "" || trigger.Annotations["dashboard_url"] == "" {
		t.Errorf("the trigger carries annotations %v, want the runbook and dashboard the "+
			"operator's alerting already knew", trigger.Annotations)
	}
	if trigger.GeneratorURL == "" {
		t.Error("the trigger carries no generator url, so the investigation cannot point " +
			"back at where the alert came from")
	}

	// And it closes when the alert does.
	ended := time.Now().UTC().Truncate(time.Second)
	gate.fire(t, resolvedAlert(alertname, began, ended))
	gate.recorder.await(t, alertname, "resolved", 1)

	deadline := time.Now().Add(30 * time.Second)
	for {
		resolved := gate.episode(t, episodeID)
		if resolved.Status == "resolved" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the episode is still %q after alertmanager resolved its alert; an "+
				"incident list that never closes is one nobody trusts", resolved.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A network blip must not double an SRE's noise. Alertmanager retries a delivery it was told
// failed, and the retry carries the identical body — which intake has already accepted.
func TestAlertmanagerGate_ARetriedDeliveryCreatesNoSecondIncident(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const alertname = "GateRetriedDelivery"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	// Intake will accept this one; Alertmanager will be told it did not.
	gate.recorder.failNextDelivery()
	gate.fire(t, firingAlert(alertname, began))
	delivered := gate.recorder.await(t, alertname, "firing", 2)

	if delivered[0].Status != http.StatusAccepted {
		t.Fatalf("the first delivery = %d, want 202", delivered[0].Status)
	}
	if delivered[1].Status != http.StatusOK {
		t.Errorf("alertmanager's retry of an identical body = %d, want 200 — a retrying "+
			"source has done nothing wrong and must be told to stop, not refused",
			delivered[1].Status)
	}
	if !bytes.Equal(delivered[0].Body, delivered[1].Body) {
		t.Fatal("alertmanager's retry carried a different body, so this asserts nothing " +
			"about redelivery")
	}

	// The same body once more, by hand: idempotence is a property of the body, not of who
	// happened to send it.
	if status := gate.replay(t, gate.secret, delivered[0].Body); status != http.StatusOK {
		t.Errorf("the same body redelivered = %d, want 200", status)
	}

	if signals := gate.signalsNamed(t, alertname); len(signals) != 1 {
		t.Errorf("one alert delivered three times produced %d signals, want 1", len(signals))
	}
	if list := gate.episodes(t); len(list.Items) != 1 {
		t.Errorf("one alert delivered three times produced %d episodes, want 1: %+v",
			len(list.Items), list.Items)
	}
	if accepted := gate.countOutcome(t, deliveryAccepted, ""); accepted != 1 {
		t.Errorf("%d deliveries were accepted, want 1", accepted)
	}
	if duplicates := gate.countOutcome(t, deliveryDuplicate, ""); duplicates != 2 {
		t.Errorf("%d deliveries were recorded as duplicates, want 2 — a retry that is not "+
			"recorded as a retry looks like a healthy source going quiet", duplicates)
	}
}

// Being turned away must never be indistinguishable from going quiet. Each refusal is its own
// recorded outcome, asserted against the body a real Alertmanager produced.
func TestAlertmanagerGate_ARefusedDeliveryIsRecordedRatherThanLost(t *testing.T) {
	gate := startAlertmanagerGate(t)
	const accepted = "GateAcceptedAlert"
	const afterDisabling = "GateAlertAfterDisabling"
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	gate.fire(t, firingAlert(accepted, began))
	genuine := gate.recorder.await(t, accepted, "firing", 1)[0]
	if genuine.Status != http.StatusAccepted {
		t.Fatalf("the delivery this test refuses variants of = %d, want 202", genuine.Status)
	}

	// A wrong secret. The operator who rotated one wants to see it, not to wonder.
	status := gate.replay(t, "not-the-webhook-secret", genuine.Body)
	if status != http.StatusUnauthorized {
		t.Errorf("a real body with a wrong secret = %d, want 401", status)
	}

	// A mangled body — half of a real one, which is what middleware rewriting a delivery
	// looks like. This is the one body Alertmanager will not produce on request.
	status = gate.replay(t, gate.secret, genuine.Body[:len(genuine.Body)/2])
	if status != http.StatusBadRequest {
		t.Errorf("a truncated real body = %d, want 400", status)
	}

	// Turning the integration off has to mean something at the door.
	gate.setEnabled(t, false)
	gate.fire(t, firingAlert(afterDisabling, began))
	refused := gate.recorder.await(t, afterDisabling, "firing", 1)[0]
	if refused.Status != http.StatusUnauthorized {
		t.Errorf("a delivery to a disabled integration = %d, want 401 — an operator who "+
			"turned it off wants deliveries refused, not merely recorded", refused.Status)
	}
	if signals := gate.signalsNamed(t, afterDisabling); len(signals) != 0 {
		t.Errorf("a disabled integration recorded %d signals, want 0", len(signals))
	}

	// Three refusals, each saying why, next to the one acceptance.
	if count := gate.countOutcome(t, deliveryAccepted, ""); count != 1 {
		t.Errorf("%d deliveries were accepted, want 1", count)
	}
	if count := gate.countOutcome(t, deliveryRejected, "unauthenticated"); count != 2 {
		t.Errorf("%d deliveries were recorded as unauthenticated, want 2 (a wrong secret "+
			"and a disabled integration)", count)
	}
	if count := gate.countOutcome(t, deliveryRejected, "malformed"); count != 1 {
		t.Errorf("%d deliveries were recorded as malformed, want 1", count)
	}
}

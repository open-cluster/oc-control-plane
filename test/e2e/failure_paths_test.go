package e2e

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

type clusterRequestGate struct {
	server    *httptest.Server
	mu        sync.Mutex
	active    bool
	entered   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (h *harness) installClusterRequestGate(t *testing.T) *clusterRequestGate {
	t.Helper()
	raw, err := os.ReadFile(h.cluster.kubeconfigPath)
	if err != nil {
		t.Fatalf("reading the real cluster's self-contained kubeconfig: %v", err)
	}
	configuration, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		t.Fatalf("decoding the real cluster's kubeconfig: %v", err)
	}
	upstream, err := url.Parse(configuration.Host)
	if err != nil {
		t.Fatalf("parsing the real cluster API address: %v", err)
	}
	transport, err := rest.TransportFor(configuration)
	if err != nil {
		t.Fatalf("retaining the real cluster's mutual-TLS transport: %v", err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = transport
	gate := &clusterRequestGate{}
	target := "/apis/apps/v1/namespaces/" + fixtureNamespace + "/deployments/" + fixtureWorkload
	gate.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gate.mu.Lock()
		block := gate.active && request.URL.Path == target
		var entered, release, cancelled chan struct{}
		if block {
			gate.active = false
			entered, release, cancelled = gate.entered, gate.release, gate.cancelled
		}
		gate.mu.Unlock()
		if block {
			close(entered)
			select {
			case <-release:
			case <-request.Context().Done():
				close(cancelled)
				return
			}
		}
		reverse.ServeHTTP(writer, request)
	}))
	t.Cleanup(gate.server.Close)
	cloned, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("cloning the Relay's kubeconfig: %v", err)
	}
	current := cloned.Contexts[cloned.CurrentContext]
	if current == nil || cloned.Clusters[current.Cluster] == nil {
		t.Fatal("the real cluster kubeconfig has no current cluster")
	}
	cluster := cloned.Clusters[current.Cluster]
	cluster.Server = gate.server.URL
	cluster.CertificateAuthority = ""
	cluster.CertificateAuthorityData = pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: gate.server.Certificate().Raw,
	})
	encoded, err := clientcmd.Write(*cloned)
	if err != nil {
		t.Fatalf("encoding the Relay's gated kubeconfig: %v", err)
	}
	path := filepath.Join(h.workDir, "gated-kubeconfig")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("writing the Relay's gated kubeconfig: %v", err)
	}
	h.relay.stop()
	h.relay.environment["RELAY_KUBECONFIG"] = path
	if err = h.relay.start(); err != nil {
		t.Fatalf("starting the real Relay behind the gated cluster API: %v", err)
	}
	t.Cleanup(func() {
		h.relay.stop()
		h.relay.environment["RELAY_KUBECONFIG"] = h.cluster.kubeconfigPath
		if restartErr := h.relay.start(); restartErr != nil {
			t.Errorf("restoring the real Relay's direct cluster access: %v", restartErr)
		}
	})
	return gate
}

func (g *clusterRequestGate) arm() (<-chan struct{}, chan<- struct{}, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = true
	g.entered = make(chan struct{})
	g.release = make(chan struct{})
	g.cancelled = make(chan struct{})
	return g.entered, g.release, g.cancelled
}

func awaitGate(t *testing.T, entered <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(jobTimeout):
		t.Fatalf("the real Relay never reached its %s execution barrier", description)
	}
}

func (h *harness) assertInFlightGuarantees(t *testing.T) {
	gate := h.installClusterRequestGate(t)

	t.Run("operator cancellation reaches a live Kubernetes read", func(t *testing.T) {
		entered, _, stopped := gate.arm()
		job := h.dispatch(t)
		awaitGate(t, entered, "active Kubernetes read")
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := h.truth.pool.Exec(context.Background(), `
			INSERT INTO investigation
			    (investigation_id, org_id, subject, window_from, window_until,
			     lease_worker, lease_expires_at)
			VALUES ($1, $2, $3, $4, $4, 'e2e-cancellation', now() + interval '5 minutes')`,
			id, organization,
			"cancel an actively executing Relay read", now); err != nil {
			t.Fatalf("creating the active investigation: %v", err)
		}
		attached, err := h.truth.pool.Exec(context.Background(), `
			UPDATE relay_job SET investigation_id = $1
			 WHERE org_id = $2 AND job_id = $3 AND status = 1`, id, organization, job)
		if err != nil {
			t.Fatalf("attaching the executing Relay read to its investigation: %v", err)
		}
		if attached.RowsAffected() != 1 {
			t.Fatalf("attaching the executing Relay read changed %d jobs", attached.RowsAffected())
		}
		base := "http://" + h.plane.httpAddress + "/api/v1"
		status, body := h.operatorRequest(t, http.MethodPost,
			base+"/investigations/"+id.String()+"/cancel", nil)
		if status != http.StatusOK {
			t.Fatalf("cancelling a live Relay investigation = %d: %s", status, body)
		}
		awaitGate(t, stopped, "cancelled real Kubernetes HTTP request")
		if result := h.awaitTerminal(t, job); result.Status != jobCancelled {
			t.Fatalf("a cancelled executing Relay job ended %s", result.Status)
		}
	})

	t.Run("a superseded execution cannot record a stale result", func(t *testing.T) {
		entered, release, _ := gate.arm()
		job := h.dispatch(t)
		awaitGate(t, entered, "fenced Kubernetes read")
		if _, err := h.truth.pool.Exec(context.Background(), `
			UPDATE relay_job SET lease_epoch = lease_epoch + 1
			 WHERE org_id = $1 AND job_id = $2 AND status = 1`, organization, job); err != nil {
			t.Fatalf("superseding the live Relay execution fence: %v", err)
		}
		close(release)
		h.await(t, "a stale live result to be refused", jobTimeout,
			func(context.Context) (bool, error) {
				logs := h.plane.logsSinceStart()
				return strings.Contains(logs, "result refused") &&
					strings.Contains(logs, job.String()), nil
			})
		record, err := h.truth.job(context.Background(), organization, job)
		if err != nil || record.Status != jobLeased {
			t.Fatalf("the stale execution changed durable job truth: %+v, %v", record, err)
		}
		if _, err = h.truth.pool.Exec(context.Background(), `
			UPDATE relay_job SET status = 0, lease_session = NULL, lease_expires_at = NULL
			 WHERE org_id = $1 AND job_id = $2 AND status = 1`, organization, job); err != nil {
			t.Fatalf("requeueing the superseded Relay execution: %v", err)
		}
		if recovered := h.awaitTerminal(t, job); recovered.Status != jobSucceeded {
			t.Fatalf("the fresh execution after fencing ended %s", recovered.Status)
		}
	})

	t.Run("an interrupted in-flight execution is recovered after reconnect", func(t *testing.T) {
		entered, _, stopped := gate.arm()
		job := h.dispatch(t)
		awaitGate(t, entered, "in-flight reconnect")
		before, err := h.truth.job(context.Background(), organization, job)
		if err != nil {
			t.Fatalf("reading the original execution fence: %v", err)
		}
		if err = h.plane.restart(context.Background()); err != nil {
			t.Fatalf("restarting the control plane during a real Relay read: %v", err)
		}
		awaitGate(t, stopped, "cluster request interrupted by the disconnected Relay")
		h.await(t, "the reconnected Relay to release its interrupted execution", reconnectTimeout,
			func(context.Context) (bool, error) {
				logs := h.plane.logsSinceStart()
				return strings.Contains(logs, "released work no relay is executing"), nil
			})
		result := h.awaitTerminalWithin(t, job, reconnectTimeout)
		if result.Status != jobSucceeded || result.LeaseEpoch <= before.LeaseEpoch {
			t.Fatalf("interrupted work was not recovered under a fresh execution fence: before=%d after=%d status=%s",
				before.LeaseEpoch, result.LeaseEpoch, result.Status)
		}
	})
}

func (h *harness) assertIdempotentResultResend(t *testing.T) {
	dropped, repeated := h.terminator.acks.arm()
	job := h.dispatch(t)
	select {
	case lost := <-dropped:
		if lost != job.String() {
			t.Fatalf("suppressed acknowledgement belonged to %s, want %s", lost, job)
		}
	case <-time.After(jobTimeout):
		t.Fatal("the real Relay result never reached the acknowledgement-drop seam")
	}
	first := h.awaitTerminal(t, job)
	if first.Status != jobSucceeded {
		t.Fatalf("the result was not durably recorded before its acknowledgement was lost: %s", first.Status)
	}
	select {
	case disposition := <-repeated:
		if disposition != relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED {
			t.Fatalf("resent result was acknowledged as %s, want already recorded", disposition)
		}
	case <-time.After(jobTimeout):
		t.Fatal("the real Relay did not resend an unacknowledged durable result")
	}
	second, err := h.truth.job(context.Background(), organization, job)
	if err != nil || second.Status != first.Status || second.LeaseEpoch != first.LeaseEpoch ||
		string(second.Result) != string(first.Result) {
		t.Fatalf("an idempotent resend changed durable result truth: first=%+v second=%+v error=%v",
			first, second, err)
	}
}

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// organization is the tenant every relay and every job in this harness belongs to. One is
// enough: tenant isolation is proven in the control plane's own suite against many, and what
// is in question here is the protocol, which does not vary by tenant.
const organization = "e2e-org"

// The compiled capabilities, named here rather than at each call site because the version is
// something several tests vary deliberately.
const (
	capabilityID      = "kubernetes.workload.runtime"
	eventsCapability  = "kubernetes.namespace.events"
	logsCapability    = "kubernetes.container.logs"
	capabilityVersion = 1
)

// jobTimeout bounds waiting for a job to reach a terminal recorded state. It is generous
// because the path is long — dispatch, a real cluster read, a result, a durable write — and
// because a timeout here should mean the path is broken, not that a container was slow.
const jobTimeout = 2 * time.Minute

// TestMain gives the compiled halves somewhere to live for the whole run. Building each
// binary once rather than per test matters: the Relay pulls a Kubernetes dependency graph,
// and per-test builds would cost more than the tests do.
func TestMain(m *testing.M) {
	remove, err := useBuildRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	remove()
	os.Exit(code)
}

// harness is one complete run: a database, a cluster, a control plane, a TLS terminator and
// a Relay, all of them real.
type harness struct {
	truth        *truth
	cluster      *cluster
	terminator   *TLSTerminator
	plane        *controlPlane
	relay        *relay
	registration uuid.UUID
	// integration is what every job in this harness reaches: a Kubernetes Integration
	// organization's Default Environment, served by the enrolled relay.
	integration uuid.UUID
	workDir     string
	// token is the bootstrap token the first Relay consumed. It is kept so a second Relay can
	// present it and be refused, which is the only way single-use consumption is observable
	// from outside this process.
	token string
}

// newHarness assembles everything and returns once a Relay has enrolled and the two halves
// are connected.
//
// It fails fast on the cheap things first — a missing Relay tree, a build error — because
// the expensive part is several minutes of containers, and discovering a typo after them is
// a poor trade.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end proof: requires a Docker daemon and both working trees")
	}
	buildBothHalves(t)
	requireDocker(t)

	ctx := context.Background()
	h := &harness{workDir: t.TempDir()}
	t.Cleanup(h.close)

	h.startDependencies(ctx, t)
	h.startControlPlane(ctx, t)
	h.startRelay(ctx, t)
	return h
}

// buildBothHalves compiles the two processes, skipping rather than failing when the Relay's
// tree is absent: a machine without it checked out has not disproved anything.
func buildBothHalves(t *testing.T) {
	t.Helper()
	if _, err := relayBinary(); err != nil {
		if errors.Is(err, errRelaySourceMissing) {
			t.Skipf("end-to-end proof: %v", err)
		}
		t.Fatalf("building the relay: %v", err)
	}
	if _, err := controlPlaneBinary(); err != nil {
		t.Fatalf("building the control plane: %v", err)
	}
}

// startDependencies brings up the database and the cluster together. They take minutes and
// have nothing to say to each other, so starting them in sequence would waste the slower
// one's time twice over.
func (h *harness) startDependencies(ctx context.Context, t *testing.T) {
	t.Helper()

	type started struct {
		truth   *truth
		cluster *cluster
		err     error
	}
	results := make(chan started, 2)

	go func() {
		store, err := startTruth(ctx)
		results <- started{truth: store, err: err}
	}()
	go func() {
		kubernetes, err := startCluster(ctx, h.workDir)
		if err == nil {
			err = kubernetes.createFixture(ctx)
		}
		results <- started{cluster: kubernetes, err: err}
	}()

	// Both results are collected before anything is judged, so a failure in one still hands
	// back the other for cleanup rather than leaking a container for the rest of the run.
	var failures []error
	for range 2 {
		result := <-results
		if result.truth != nil {
			h.truth = result.truth
		}
		if result.cluster != nil {
			h.cluster = result.cluster
		}
		if result.err != nil {
			failures = append(failures, result.err)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("bringing up the dependencies: %v", errors.Join(failures...))
	}
}

// requireDocker skips when there is no container runtime, and only then.
//
// The distinction is the whole reason this exists. A machine with no Docker has not
// disproved anything, so skipping is honest. A machine with Docker where a container failed
// to start has found something — a pinned image that no longer exists, a daemon out of disk —
// and reporting that as a skip would let a run that proved nothing read as a run that passed.
func requireDocker(t *testing.T) {
	t.Helper()

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		noContainerRuntime(t, "no container runtime", err)
	}
	defer func() { _ = provider.Close() }()

	if err = provider.Health(context.Background()); err != nil {
		noContainerRuntime(t, "the container runtime is not healthy", err)
	}
}

// startControlPlane puts the process behind the TLS terminator the Relay will dial.
//
// The terminator comes first because the pin it generates is what the control plane must
// advertise at enrolment. A fresh certificate per run is deliberate: a fixed one would let a
// run pass by trusting a key an earlier run left behind.
func (h *harness) startControlPlane(ctx context.Context, t *testing.T) {
	t.Helper()

	plane, err := newControlPlane(h.workDir, h.truth.dsn)
	if err != nil {
		t.Fatalf("preparing the control plane: %v", err)
	}
	h.plane = plane

	terminator, err := StartTLSTerminator("127.0.0.1", plane.relayAddress)
	if err != nil {
		t.Fatalf("starting the tls terminator: %v", err)
	}
	h.terminator = terminator

	if err = plane.start(ctx, terminator.SPKIPin); err != nil {
		t.Fatalf("starting the control plane: %v", err)
	}
	// The schema exists only after the control plane has applied its migrations, so the
	// harness connects to a database it did not create and does not migrate.
	if err = h.truth.connect(ctx); err != nil {
		t.Fatalf("connecting to the database: %v", err)
	}
}

// startRelay issues a bootstrap token, runs the Relay, and waits for an identity to appear.
func (h *harness) startRelay(ctx context.Context, t *testing.T) {
	t.Helper()

	token, err := h.truth.issueBootstrapToken(ctx, organization)
	if err != nil {
		t.Fatalf("issuing a bootstrap token: %v", err)
	}
	h.token = token

	relay, err := newRelay(h.installation("primary", token))
	if err != nil {
		t.Fatalf("preparing the relay: %v", err)
	}
	h.relay = relay

	if err = relay.start(); err != nil {
		t.Fatalf("starting the relay: %v", err)
	}
	h.registration = h.awaitRegistration(t)

	integration, err := h.truth.kubernetesIntegration(ctx, organization, h.registration)
	if err != nil {
		t.Fatalf("creating the kubernetes integration: %v", err)
	}
	h.integration = integration
}

// installation describes a Relay pointed at this harness's control plane and cluster. Every
// Relay in a run differs only in its name and the token it presents.
func (h *harness) installation(name, token string) relayInstallation {
	return relayInstallation{
		Name:                name,
		WorkDir:             h.workDir,
		Token:               token,
		ControlPlaneAddress: h.terminator.Address,
		SPKIPin:             h.terminator.SPKIPin,
		Organization:        organization,
		KubeconfigPath:      h.cluster.kubeconfigPath,
	}
}

// awaitRegistration waits for an enrolment to be recorded, which is the first thing that
// requires both halves to agree about anything.
func (h *harness) awaitRegistration(t *testing.T) uuid.UUID {
	t.Helper()

	var registration uuid.UUID
	h.await(t, "a relay to enrol", time.Minute, func(ctx context.Context) (bool, error) {
		id, found, err := h.truth.registration(ctx, organization)
		registration = id
		return found, err
	})
	return registration
}

// dispatch enqueues a job that reads the fixture workload.
func (h *harness) dispatch(t *testing.T) uuid.UUID {
	t.Helper()
	return h.dispatchRead(t, fixtureNamespace, fixtureWorkload)
}

// dispatchRead enqueues a workload-runtime job for the enrolled relay and returns its
// identity. The workload it names need not exist: what a read of an absent workload reports
// is itself a property worth proving.
func (h *harness) dispatchRead(t *testing.T, namespace, workload string) uuid.UUID {
	t.Helper()
	return h.dispatchVersion(t, capabilityVersion, namespace, workload)
}

// dispatchVersion enqueues a job at a stated capability version.
//
// It can name a version no Relay has, which is the only way to reach that path from outside:
// the control plane sends whatever version the job carries, so writing the job is how a
// too-old Relay is simulated without pretending to be one.
func (h *harness) dispatchVersion(
	t *testing.T, version uint32, namespace, workload string,
) uuid.UUID {
	t.Helper()

	arguments, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
				Namespace:    namespace,
				WorkloadKind: relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
				WorkloadName: workload,
				MaxPods:      10,
			},
		},
	})
	if err != nil {
		t.Fatalf("encoding job arguments: %v", err)
	}

	return h.enqueueCapability(t, capabilityID, version, arguments)
}

// enqueueCapability enqueues one job for any compiled capability.
func (h *harness) enqueueCapability(
	t *testing.T, capability string, version uint32, arguments []byte,
) uuid.UUID {
	t.Helper()

	id, err := h.truth.enqueueJob(context.Background(), organization,
		h.registration, h.integration, capability, version, arguments)
	if err != nil {
		t.Fatalf("enqueueing the job: %v", err)
	}
	return id
}

// dispatchEvents enqueues a namespace-events read over the window given.
func (h *harness) dispatchEvents(
	t *testing.T, namespace string, since time.Duration, maxEvents uint32,
) uuid.UUID {
	t.Helper()

	now := time.Now().UTC()
	arguments, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{
				Namespace:   namespace,
				WindowStart: timestamppb.New(now.Add(-since)),
				WindowEnd:   timestamppb.New(now.Add(time.Minute)),
				MaxEvents:   maxEvents,
			},
		},
	})
	if err != nil {
		t.Fatalf("encoding events arguments: %v", err)
	}
	return h.enqueueCapability(t, eventsCapability, capabilityVersion, arguments)
}

// dispatchLogs enqueues a container-logs read.
func (h *harness) dispatchLogs(
	t *testing.T, namespace, pod, container string, previous bool,
	maxLines, maxBytes uint32,
) uuid.UUID {
	t.Helper()

	arguments, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{
				Namespace:     namespace,
				PodName:       pod,
				ContainerName: container,
				Previous:      previous,
				MaxLines:      maxLines,
				MaxBytes:      maxBytes,
			},
		},
	})
	if err != nil {
		t.Fatalf("encoding logs arguments: %v", err)
	}
	return h.enqueueCapability(t, logsCapability, capabilityVersion, arguments)
}

// awaitTerminal waits for a job to reach a recorded terminal state and returns it.
func (h *harness) awaitTerminal(t *testing.T, id uuid.UUID) jobRecord {
	t.Helper()
	return h.awaitTerminalWithin(t, id, jobTimeout)
}

// awaitTerminalWithin is awaitTerminal with a stated budget, for the cases that have to
// absorb a reconnect and would otherwise need their own copy of the polling loop.
func (h *harness) awaitTerminalWithin(
	t *testing.T, id uuid.UUID, budget time.Duration,
) jobRecord {
	t.Helper()

	var record jobRecord
	h.await(t, "job "+id.String()+" to reach a terminal state", budget,
		func(ctx context.Context) (bool, error) {
			found, err := h.truth.job(ctx, organization, id)
			record = found
			return found.Status.terminal(), err
		})
	return record
}

// await polls until the condition holds, and reports both halves' output when it does not.
//
// Reporting both is the whole point: a failure here has two candidate causes by construction,
// and a message that named neither would send the reader to bisect two codebases.
func (h *harness) await(
	t *testing.T, description string, budget time.Duration,
	condition func(context.Context) (bool, error),
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var lastErr error
	for {
		done, err := condition(ctx)
		if done {
			return
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waited %s for %s and it did not happen (last error: %v)\n\n%s",
				budget, description, lastErr, h.diagnostics())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// diagnostics renders what each half said, labelled, so a failure starts from evidence.
func (h *harness) diagnostics() string {
	return fmt.Sprintf("--- control plane ---\n%s\n--- relay ---\n%s",
		h.plane.logs(), h.relay.logs())
}

func (h *harness) close() {
	h.relay.stop()
	h.plane.stop()
	if h.terminator != nil {
		_ = h.terminator.Close()
	}
	h.truth.close()
	h.cluster.close()
}

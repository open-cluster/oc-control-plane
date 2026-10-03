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

const (
	capabilityID      = "kubernetes.workload.runtime"
	eventsCapability  = "kubernetes.namespace.events"
	logsCapability    = "kubernetes.container.logs"
	capabilityVersion = 1
)

const jobTimeout = 2 * time.Minute

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

type harness struct {
	truth        *truth
	cluster      *cluster
	terminator   *TLSTerminator
	plane        *controlPlane
	relay        *relay
	registration uuid.UUID
	integration  uuid.UUID
	workDir      string
	token        string
}

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

func (h *harness) startControlPlane(ctx context.Context, t *testing.T) {
	t.Helper()

	model := startInvestigationModel(t)
	plane, err := newControlPlane(h.workDir, h.truth.dsn, model.URL)
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
	if err = h.truth.connect(ctx); err != nil {
		t.Fatalf("connecting to the database: %v", err)
	}
}

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

func (h *harness) dispatch(t *testing.T) uuid.UUID {
	t.Helper()
	return h.dispatchRead(t, fixtureNamespace, fixtureWorkload)
}

func (h *harness) dispatchRead(t *testing.T, namespace, workload string) uuid.UUID {
	t.Helper()
	return h.dispatchVersion(t, capabilityVersion, namespace, workload)
}

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

func (h *harness) awaitTerminal(t *testing.T, id uuid.UUID) jobRecord {
	t.Helper()
	return h.awaitTerminalWithin(t, id, jobTimeout)
}

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

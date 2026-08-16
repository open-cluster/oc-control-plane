package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The two reads that carry the answer in most Kubernetes workload failures, proven across the
// real protocol against a real cluster.
//
// What is proven here and nowhere else is that the COMPLETENESS BASIS arrives intact. Both
// executors have unit tests against a fake port, and those prove the executor computes the
// basis; only this proves it survives encoding, the stream, the recording transaction, and the
// column it lands in. The central certificate logic depends on exactly that survival, and a
// field lost anywhere along the path would look, from the control plane's side, like a cluster
// where nothing happened.
//
// There is no parity oracle for either capability — the frozen .NET reference has no events
// reader and no log reader — so the refusal paths get as much attention as the happy ones.

// restartTimeout bounds waiting for the crashing fixture to die and come back. A container
// that echoes and exits restarts quickly, but the kubelet's backoff grows, so the budget is
// generous.
const restartTimeout = 3 * time.Minute

// recordedResultOf decodes the whole capability payload as it was durably stored. Tests that only
// need one capability's half go through the helpers below; this exists for the ones whose subject
// is the envelope itself, such as what redaction reported.
func recordedResultOf(t *testing.T, record jobRecord) *relayv1.CapabilityResult {
	t.Helper()

	result := &relayv1.CapabilityResult{}
	if err := proto.Unmarshal(record.Result, result); err != nil {
		t.Fatalf("decoding the recorded result: %v", err)
	}
	return result
}

func eventsResultOf(t *testing.T, record jobRecord) *relayv1.KubernetesNamespaceEventsResultV1 {
	t.Helper()

	result := recordedResultOf(t, record)
	events := result.GetKubernetesNamespaceEventsV1()
	if events == nil {
		t.Fatalf("the recorded result is not a namespace-events result: %+v", result)
	}
	return events
}

func logsResultOf(t *testing.T, record jobRecord) *relayv1.KubernetesContainerLogsResultV1 {
	t.Helper()

	result := recordedResultOf(t, record)
	logs := result.GetKubernetesContainerLogsV1()
	if logs == nil {
		t.Fatalf("the recorded result is not a container-logs result: %+v", result)
	}
	return logs
}

// The cluster's own account of what it did crosses the protocol with its completeness basis
// intact. A namespace that has just started three workloads has a great deal to say, so this
// asserts on the shape of the answer rather than on any particular event.
func TestProof_TheClusterSaysWhatItDidAndTheBasisSurvives(t *testing.T) {
	h := newHarness(t)

	// Half the attested horizon, not all of it. A window exactly one horizon wide is a boundary
	// value: the Relay judges it against its own clock at execution time, which is necessarily
	// later than the clock that built the window, so such a window legitimately reaches past the
	// horizon by however long dispatch took. Asserting on that would be asserting on latency.
	record := h.awaitTerminal(t, h.dispatchEvents(t, fixtureNamespace, 30*time.Minute, 100))
	if record.Status != jobSucceeded {
		t.Fatalf("the events job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	events := eventsResultOf(t, record)
	if events.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS {
		t.Fatalf("the events read reported %v, want SUCCESS", events.GetOutcome())
	}
	if events.GetReturnedEventCount() == 0 {
		t.Fatal("a namespace that has just started three workloads said nothing, which means " +
			"the read reached the cluster and came back with a window that admitted nothing")
	}
	if !events.GetComplete() {
		t.Error("a bounded page that fitted must report a complete read; without it no " +
			"certified absence can ever be minted")
	}
	if events.GetWindowPredatesRetention() {
		t.Error("a window well inside the attested horizon must not be flagged as reaching " +
			"past it; an investigation would record a coverage gap that is not there")
	}
	if events.GetAppliedRetentionHorizon().AsDuration() <= 0 {
		t.Error("the horizon the retention judgement rests on must arrive, or the flag beside " +
			"it cannot be interpreted")
	}
	if events.GetAppliedMaxEvents() == 0 || events.GetReadAt() == nil {
		t.Errorf("the completeness basis is incomplete: bound=%d read_at=%v",
			events.GetAppliedMaxEvents(), events.GetReadAt())
	}

	// Every event carries the object it is about and something the cluster said. Both are
	// untrusted text and both are what an investigator reads.
	for _, event := range events.GetEvents() {
		if event.GetInvolvedObject().GetName() == "" {
			t.Errorf("an event arrived with no object: %+v", event)
		}
		if event.GetLastSeenAt() == nil {
			t.Errorf("an event arrived with no time, so it cannot be placed on a timeline: %+v",
				event)
		}
	}
}

// A window entirely in the past says so, and that flag is what stops an empty result being
// read as an absence. It is reported rather than inferred, because nobody but the operator
// knows their apiserver's event TTL.
func TestProof_AWindowBeyondRetentionSaysSoRatherThanLookingEmpty(t *testing.T) {
	h := newHarness(t)

	// A window that began a day ago, against a Relay attesting the Kubernetes default horizon
	// of one hour.
	record := h.awaitTerminal(t, h.dispatchEvents(t, fixtureNamespace, 24*time.Hour, 100))
	if record.Status != jobSucceeded {
		t.Fatalf("the events job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	events := eventsResultOf(t, record)
	if !events.GetWindowPredatesRetention() {
		t.Fatal("a window reaching past the attested retention horizon must say so; without " +
			"it an empty result there cannot be told from nothing having happened")
	}
}

// A namespace with nothing in it returns an empty COMPLETE read, which is what a certified
// absence is minted from. The distinction between this and a refused read is the one the whole
// truth model rests on.
func TestProof_AnEmptyNamespaceIsACompleteReadRatherThanAFailure(t *testing.T) {
	h := newHarness(t)

	record := h.awaitTerminal(t, h.dispatchEvents(t, "e2e-nothing-here", time.Hour, 100))
	if record.Status != jobSucceeded {
		t.Fatalf("the events job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	events := eventsResultOf(t, record)
	if events.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS {
		t.Fatalf("an empty namespace reported %v, want SUCCESS", events.GetOutcome())
	}
	if events.GetReturnedEventCount() != 0 {
		t.Fatalf("a namespace with nothing in it returned %d events",
			events.GetReturnedEventCount())
	}
	if !events.GetComplete() {
		t.Fatal("an empty read that reached the cluster and saw everything is complete; " +
			"reporting it otherwise makes a certified absence impossible")
	}
}

// A running container's own words cross the protocol, with their timestamps.
func TestProof_AContainerSaysWhatItSaidAndTheLinesCarryTheirTimes(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	pod := h.awaitPod(t, ctx, talkativeWorkload)

	record := h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, talkativeWorkload, false, 100, 65536))
	if record.Status != jobSucceeded {
		t.Fatalf("the logs job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	logs := logsResultOf(t, record)
	if logs.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("the log read reported %v, want SUCCESS", logs.GetOutcome())
	}
	if !containsMarker(logs.GetLines(), livingMarker) {
		t.Fatalf("the container's own words did not arrive: %+v", logs.GetLines())
	}
	if !logs.GetComplete() {
		t.Error("a short log that fitted both bounds is a complete read")
	}
	if logs.GetWithheldByteCount() != 0 {
		t.Errorf("a complete read withheld %d bytes", logs.GetWithheldByteCount())
	}
	if logs.GetAppliedMaxLines() == 0 || logs.GetAppliedMaxBytes() == 0 || logs.GetReadAt() == nil {
		t.Errorf("the completeness basis is incomplete: lines=%d bytes=%d read_at=%v",
			logs.GetAppliedMaxLines(), logs.GetAppliedMaxBytes(), logs.GetReadAt())
	}
	for _, line := range logs.GetLines() {
		if line.GetAt() == nil {
			t.Errorf("a line arrived with no time, so it cannot be placed on a timeline: %q",
				line.GetContent())
		}
	}
}

// The container that DIED is the one that explains the failure, and the one that replaced it
// is usually silent. This is the read the whole capability exists for.
func TestProof_ThePreviousContainerIsWhatExplainsTheFailure(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	pod, err := h.cluster.awaitRestarted(ctx, crashingWorkload)
	if err != nil {
		t.Fatalf("waiting for the crashing fixture: %v\n\n%s", err, h.diagnostics())
	}

	record := h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, crashingWorkload, true, 100, 65536))
	if record.Status != jobSucceeded {
		t.Fatalf("the logs job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	logs := logsResultOf(t, record)
	if logs.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("the previous-container read reported %v, want SUCCESS", logs.GetOutcome())
	}
	if !containsMarker(logs.GetLines(), dyingMarker) {
		t.Fatalf("the dead container's last words did not arrive: %+v", logs.GetLines())
	}
}

// A container that has never restarted has no previous instance, and that is a different fact
// from a wrong container name. An investigator told the wrong one looks in the wrong place.
func TestProof_APreviousReadOnAContainerThatNeverDiedIsItsOwnOutcome(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	pod := h.awaitPod(t, ctx, talkativeWorkload)

	record := h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, talkativeWorkload, true, 100, 65536))
	if record.Status != jobSucceeded {
		t.Fatalf("the logs job reached %s, want succeeded\n\n%s", record.Status, h.diagnostics())
	}

	logs := logsResultOf(t, record)
	if logs.GetOutcome() !=
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_PREVIOUS_CONTAINER_NOT_FOUND {
		t.Fatalf("a container that never died reported %v, want PREVIOUS_CONTAINER_NOT_FOUND",
			logs.GetOutcome())
	}
	if logs.GetComplete() {
		t.Error("a not-found is never a complete read, and must never mint an absence")
	}
}

// A pod that is not there, and a container that is not on a pod that is, are typed outcomes
// rather than failures. The difference between "not there" and "could not look" is what a
// certified absence rests on.
func TestProof_MissingPodsAndContainersAreTypedOutcomes(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	pod := h.awaitPod(t, ctx, talkativeWorkload)

	missingPod := logsResultOf(t, h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, "no-such-pod", "api", false, 100, 65536)))
	if missingPod.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_POD_NOT_FOUND {
		t.Errorf("a missing pod reported %v, want POD_NOT_FOUND", missingPod.GetOutcome())
	}

	missingContainer := logsResultOf(t, h.awaitTerminal(t,
		h.dispatchLogs(t, fixtureNamespace, pod, "no-such-container", false, 100, 65536)))
	if missingContainer.GetOutcome() !=
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_CONTAINER_NOT_FOUND {
		t.Errorf("a missing container reported %v, want CONTAINER_NOT_FOUND",
			missingContainer.GetOutcome())
	}
	for name, logs := range map[string]*relayv1.KubernetesContainerLogsResultV1{
		"missing pod":       missingPod,
		"missing container": missingContainer,
	} {
		if logs.GetComplete() {
			t.Errorf("a %s reported a complete read; a not-found is never absence evidence", name)
		}
	}
}

// A capability version no Relay has is refused and never executed. Both new capabilities get
// this test because the one it exists for found a real defect: before it, the Relay dispatched
// every assignment to its single executor without reading the capability or the version at all.
func TestProof_AVersionNoRelayHasIsRefusedForBothNewCapabilities(t *testing.T) {
	h := newHarness(t)

	events, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{},
		},
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	logs, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{},
		},
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	for name, arguments := range map[string][]byte{
		eventsCapability: events,
		logsCapability:   logs,
	} {
		// Version 2 of a frozen schema means semantics no build has. The control plane refuses
		// it before dispatch and the Relay would refuse it on receipt; either way it must reach
		// a recorded terminal failure rather than being executed under v1's meaning.
		record := h.awaitTerminal(t, h.enqueueCapability(t, name, 2, arguments))
		if record.Status != jobFailed {
			t.Errorf("%s v2 reached %s, want failed\n\n%s", name, record.Status, h.diagnostics())
		}
	}
}

// awaitPod waits for a fixture workload's pod to be running and returns its name.
// Running rather than existing: a log read racing container start is answered with a
// typed failure, and the tests waiting here are about what a started container said.
func (h *harness) awaitPod(t *testing.T, ctx context.Context, workload string) string {
	t.Helper()

	var name string
	h.await(t, "a running pod for "+workload, fixtureTimeout, func(context.Context) (bool, error) {
		found, err := h.cluster.runningPodFor(ctx, workload)
		if err != nil {
			return false, err
		}
		name = found
		return true, nil
	})
	return name
}

func containsMarker(lines []*relayv1.KubernetesLogLine, marker string) bool {
	for _, line := range lines {
		if strings.Contains(line.GetContent(), marker) {
			return true
		}
	}
	return false
}
